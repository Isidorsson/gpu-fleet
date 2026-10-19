# Architecture

How the pieces connect, how a node moves through its lifecycle, and the
concurrency rules that keep a shared fleet consistent. The per-component
contracts live in their own documents, linked from each section.

## How the pieces connect

```
 gpu-agent (Rust) <-/metrics- Prometheus --alerts--> Alertmanager
 one per node                     |                       |
                               /metrics            POST /v1/alerts
                                  v                       v
 chaos --REST--------->+------------------------------------------+      +----------+
                       |            control plane (Go)            |<---->| Postgres |
                       |        API | workers | reconciler        |      +----------+
                       +--------------------+---------------------+
                                            | Redfish + deploy agent
                                            v
                                         bmcsim
```

| From | To | Over | Purpose |
|---|---|---|---|
| Prometheus | gpu-agent | `GET /metrics` on :9400 | per-GPU health, dcgm-exporter metric names |
| Prometheus | control plane | `GET /metrics` | fleet, task, capacity, attestation and remediation metrics |
| Prometheus | Alertmanager | alerts | rules in `deploy/prometheus/alerts.yml` |
| Alertmanager | control plane | `POST /v1/alerts`, bearer token | only `GPUFatalXid` is routed here |
| provisioner workers | bmcsim | Redfish and the in-band deploy agent | power, PXE, tasks, attestation evidence |
| control plane | Postgres | SQL | all state: nodes, leases, reservations, attestations, remediation log |
| chaos | control plane | REST | registers nodes, books reservations, sends `repaired` |
| chaos | bmcsim | `/sim/systems/{id}/tamper` | plants implants |
| chaos | gpu-agent | `POST /fake/xid` | injects GPU faults |

Components:

- **Control plane** (`controlplane/cmd/controlplane`): HTTP API, provisioner
  workers, reservation reconciler, attestation service, remediation webhook and
  `/metrics`, in one process. Replicas are safe: workers claim work through
  Postgres leases and the reconciler elects a leader.
- **bmcsim** (`controlplane/cmd/bmcsim`): a fleet of simulated BMCs with seeded
  failure injection. Serves the Redfish subset, the in-band deploy agent and
  the attester, per [bmc-protocol.md](bmc-protocol.md) and
  [attestation.md](attestation.md).
- **chaos** (`controlplane/cmd/chaos`): keeps the demo busy, per
  [demo.md](demo.md).
- **gpu-agent** (`agent/`): one per host, exports GPU health as Prometheus
  metrics, per [metrics.md](metrics.md). It runs on a fake GPU backend; the
  NVML backend is not implemented yet. `unsafe` is forbidden crate-wide.
- **attestref** (`controlplane/cmd/attestref`): prints reference values that
  accept an untampered bmcsim started with the same `--seed`.
- **Postgres**: the only shared state. Tables: `nodes`, `node_events`,
  `reservations`, `reservation_nodes`, `attestation_nonces`, `attestations`,
  `remediation_actions`.

The control plane keeps a pure core and pushes I/O to the edges. `node`
(state machine), `placement`, `attest` (verification) and `remediation`
(`Decide`) do no I/O and are tested without a database. `store` is the only
package that talks to Postgres. `provisioner`, `reservation`, `attestsvc` and
`api` orchestrate on top, and `redfish` and `deployagent` are the clients for
the machines.

## Node lifecycle

```
racked -> inspecting -> available -> provisioning -> active
              ^                           |            |
              |                    provision_failed  release
              |                           v            v
              +--- wipe_succeeded ---- wiping <--------+
```

Failures and faults end in `quarantined`:

```
inspecting --inspection_failed, attestation_failed--> quarantined
wiping     --wipe_failed----------------------------> quarantined
any powered state --hardware_fault-------------------> quarantined --repaired--> wiping
```

"Powered" means every state but `racked`. `provision` moves `available` to
`provisioning` and needs a tenant; no other event may carry one.

Invariants, enforced by tests in `controlplane/internal/node`:

- A node only becomes `available` through `inspecting`, which includes
  attestation. That covers every return to the pool after a wipe.
- A node only leaves a tenant through `wiping` (or `quarantined`). The tenant
  is set while its data may still be on the disks, so it survives quarantine
  and is cleared only by a successful wipe.

Postgres backs the state machine up with `CHECK` constraints, so a bug above
it cannot write an impossible row: `state` is one of the seven states, a node
in `provisioning` or `active` always has a tenant, a reservation implies a
tenant, and a lease owner and its expiry are set together.

Each event runs in one transaction holding a row lock on the node, so
concurrent events on the same node are serialized and the audit log
(`node_events`) always matches the node's state.

## Provisioning, leases and fencing

Provisioner workers run inside the control plane (`PROVISIONER_WORKERS`,
default 4). Each claims a node that needs work (`racked`, `inspecting`,
`provisioning`, `wiping`) with `SELECT ... FOR UPDATE SKIP LOCKED` and a lease,
drives it through Redfish (PXE once, power cycle) and the in-band deploy agent,
and records the result.

- Leases last 30 seconds and are renewed while a task runs. A crashed worker's
  nodes are reclaimed when the lease expires. A node whose workers keep dying
  mid-task sorts behind nodes that make progress.
- Results are fenced: an event is only applied while the worker still holds
  the lease, checked under the node's row lock against the database clock. A
  stalled worker cannot overwrite its successor.
- Each BMC or agent call is tried up to 5 times with a backoff that starts at
  500 ms and doubles. Exhausted retries, non-transient errors and task
  timeouts (10 minutes) quarantine the node with `hardware_fault`.
- After an inspect task succeeds the worker attests the node and only then
  powers it off.

`bmcsim` simulates a fleet of BMCs per [bmc-protocol.md](bmc-protocol.md) with
seeded failure injection, so the whole flow runs on a laptop.

## Capacity reservations

Customers reserve N whole nodes for a time window, placed close together on the
fabric: `same_rack`, `same_pod` or `any`. Placement is a pure function, so the
policy rules are unit-tested without a database. Contract and API:
[reservations.md](reservations.md).

- No node is ever double-booked. A Postgres exclusion constraint on
  `(node_id, period)` enforces it, and a booking that loses a race retries
  placement, up to 5 attempts. If every attempt loses, the API answers 503.
- A reconciler provisions the nodes when a reservation starts, swaps in
  replacements for nodes quarantined in the meantime, and releases (wipes) them
  when it ends. It holds a Postgres advisory lock, so one replica acts at a
  time, ticks every 5 seconds, and every step is idempotent: a crashed tick is
  redone by the next one.
- When the fleet cannot satisfy a request the API answers 409 with the
  `largest` block available.

## Tenant attestation

Bare-metal tenants have root, so a previous tenant could leave an implant that
survives a disk wipe. Every inspection, including the one after every wipe,
ends with remote attestation before a node can become `available`.

- The node's in-band agent returns a quote over its measured-boot PCRs
  (firmware, BMC, bootloader, kernel) signed with the attestation key enrolled
  at racking, the event log, and per-GPU reports (VBIOS, confidential computing
  mode) signed by device keys certified by the GPU vendor root.
- The verifier checks a single-use nonce, the quote signature, that the event
  log replays to the quoted PCRs, every digest against the
  [reference values](../deploy/attestation/reference.json), and the GPU chain.
- A failure moves the node to `quarantined` (`attestation_failed`). Every
  result is signed by the control plane, so tenants can verify it offline.

The TPM and GPU are simulated in software. [attestation.md](attestation.md)
has the threat model and maps every simulated piece to its TPM 2.0, NVIDIA and
IETF RATS equivalent.

## Automated remediation

A fatal GPU XID quarantines the node without a human sending an event:

```
agent (node_id label) -> Prometheus GPUFatalXid -> Alertmanager
  -> POST /v1/alerts (bearer token) -> control plane -> node quarantined
  -> reconciler replaces it for the tenant, if one holds it
```

Alertmanager routes `GPUFatalXid` to the control plane webhook and drops
everything else. Resolved alerts never un-quarantine a node: repair is a human
decision (the `repaired` event). At most 5 nodes per 10 minutes are quarantined
this way, counted under a transaction-scoped advisory lock so replicas share
one limit. Beyond that the outcome is `rate_limited` and the
`RemediationRateLimited` alert pages instead, so one noisy rule cannot empty
the fleet. Every handled alert is recorded with its outcome. Contract:
[remediation.md](remediation.md).

## Design decisions

- **Pure core, constraints as backstop.** The lifecycle, placement,
  verification and remediation policy are pure functions tested without a
  database. Postgres `CHECK` constraints repeat the invariants that matter, as
  defense in depth.
- **Concurrency lives in Postgres.** Row locks serialize events per node,
  `SKIP LOCKED` lets workers claim work without contending, lease fencing
  rejects stale results, an exclusion constraint rules out double booking and
  advisory locks elect the reconciler leader and share the remediation limit.
- **Fail closed.** A node is unusable until it has passed attestation. Nonces
  are single use with a 60 second TTL, every verification failure is reported
  rather than just the first, and the reference policy must state
  `require_cc_mode` explicitly, so a policy file cannot disable the check by
  omission.
- **Read-only public listener by construction.** The control plane can serve a
  second listener (`PUBLIC_ADDR`) whose handler is `api.ReadOnly` wrapped
  around the same API and passes only GET and HEAD. The full API stays on a
  private port. See [deploy/railway/README.md](../deploy/railway/README.md).
- **Contracts first, simulators everywhere.** Each component has a contract
  document that both sides implement independently, so client tests against
  the simulator verify the contract. bmcsim, the fake GPU backend and chaos
  mean the whole system runs and is tested without hardware.
- **Go control plane, Rust agent, one repository.**
  [ADR 1](adr/0001-go-control-plane-rust-agent.md).
