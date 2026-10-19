# gpu-fleet

A small control plane for bare-metal GPU clouds: node lifecycle, capacity
reservations and GPU health, runnable on a laptop against a simulated fleet.

```
controlplane/   Go: lifecycle state machine, provisioning + capacity APIs
agent/          Rust: per-node GPU health agent (NVML, fake backend for dev)
deploy/         docker compose: Postgres, GPU agents, Prometheus + alerts, Alertmanager, Grafana, demo profile
docs/adr/       architecture decision records
```

## Quick demo

```bash
docker compose -f deploy/compose.yaml --profile demo up -d --build
```

Needs Docker with Compose v2 and free host ports 3000, 5432, 8080, 8443, 9090,
9093 and 19401 to 19404 (stop a host-run control plane first, it also uses
8080). The first run builds the Go and Rust images, which takes a few minutes.

It starts everything in containers and keeps it busy: a simulated 50-node
fleet is racked, inspected and attested (a few minutes), tenants book
reservations, and a chaos service plants implants and GPU faults that the
platform catches and repairs on its own. Contract: [docs/demo.md](docs/demo.md).

- Grafana, anonymous, opens on the GPU Fleet dashboard: http://localhost:3000
- Prometheus and its alerts: http://localhost:9090
- Alertmanager: http://localhost:9093
- Control plane API: `curl -s localhost:8080/v1/capacity`
- What the chaos service is doing:
  `docker compose -f deploy/compose.yaml --profile demo logs -f chaos`

Stop it with `docker compose -f deploy/compose.yaml --profile demo down -v`.
The fleet is stored in the `pgdata` volume while bmcsim keeps its state in
memory, so drop the volume (`-v`) to start the next run clean. The
`demo-smoke` CI job runs the `up` command and waits until 40 nodes are
`available`.

## Node lifecycle

```
racked -> inspecting -> available -> provisioning -> active
              ^                           |            |
              |                       (failed)     (release)
              |                           v            v
              +---------------------- wiping <---------+
                                          ^
any powered state --hardware_fault--> quarantined --repaired--+
```

Invariants (enforced by tests in `controlplane/internal/node`):

- A node only becomes `available` after passing inspection.
- A node only leaves a tenant through `wiping` (or `quarantined`).

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/nodes` | `{"id","pod","rack","bmc"}`, registers a node in `racked`; `bmc` is the BMC base URL (absolute http/https URL, no credentials, query or fragment) |
| `GET` | `/v1/nodes?state=available` | `state` filter is optional |
| `GET` | `/v1/nodes/{id}` | |
| `POST` | `/v1/nodes/{id}/events` | `{"event":"provision","tenant":"acme"}` |
| `GET` | `/v1/nodes/{id}/events` | audit log, oldest first |

Errors: 400 malformed request, 404 unknown node, 409 invalid transition or
duplicate node, 422 unknown event or tenant rule violated.

Each event runs in one transaction holding a row lock on the node, so
concurrent events on the same node are serialized and the audit log always
matches the node's state.

## Provisioning

Provisioner workers run inside the control plane. Each claims a node that
needs work (`racked`, `inspecting`, `provisioning`, `wiping`) with
`SELECT ... FOR UPDATE SKIP LOCKED` and a lease, drives it through Redfish
(PXE once, power cycle) and the in-band deploy agent, and records the result.

- Leases are renewed while a task runs. A crashed worker's nodes are
  reclaimed when the lease expires.
- Results are fenced: an event is only applied while the worker still holds
  the lease, so a stalled worker cannot overwrite its successor.
- Transient BMC errors are retried with backoff. Exhausted retries,
  non-transient errors and task timeouts quarantine the node.

`bmcsim` simulates a fleet of BMCs per [docs/bmc-protocol.md](docs/bmc-protocol.md)
with seeded failure injection, so the whole flow runs on a laptop.

## Capacity reservations

Reserve whole nodes for a time window, placed close together on the fabric:
`same_rack`, `same_pod` or `any`. A `starts_at` in the past means now. If the
fleet cannot satisfy the request the API answers 409 with the `largest` block
available. Rules and contract: [docs/reservations.md](docs/reservations.md).

- No node is ever double-booked: a Postgres exclusion constraint on
  `(node_id, period)` enforces it, and concurrent bookings that lose the race
  retry placement.
- A leader-elected reconciler (Postgres advisory lock) provisions the nodes
  when a reservation starts, swaps in replacements for nodes quarantined in
  the meantime, and releases (wipes) them when it ends.

```bash
curl -s -X POST localhost:8080/v1/reservations -H 'Content-Type: application/json' \
  -d '{"tenant":"acme","count":8,"policy":"same_rack","starts_at":"2026-11-01T09:00:00Z","ends_at":"2026-11-01T17:00:00Z"}'
```

```bash
curl -s localhost:8080/v1/capacity
```

## Tenant attestation

Bare-metal tenants have root, so a previous tenant could leave an implant that
survives a disk wipe. Every inspection, including the one after every wipe,
ends with remote attestation before a node can become `available`:

- The node's in-band agent returns a quote over its measured-boot PCRs
  (firmware, BMC, bootloader, kernel) signed with the attestation key enrolled
  at racking, the event log, and per-GPU reports (VBIOS, confidential
  computing mode) signed by device keys certified by the GPU vendor root.
- The verifier checks a single-use nonce, the quote signature, that the event
  log replays to the quoted PCRs, every digest against
  [reference values](deploy/attestation/reference.json), and the GPU chain.
- A failure moves the node to `quarantined` (`attestation_failed`). Every
  result is signed by the control plane, so tenants can verify it offline:

```bash
curl -s localhost:8080/v1/nodes/node-001/attestation
```

```bash
curl -s localhost:8080/v1/attestation/keys
```

The TPM and GPU are simulated in software; [docs/attestation.md](docs/attestation.md)
has the threat model and maps every simulated piece to TPM 2.0, NVIDIA and
IETF RATS equivalents. Inject an implant into a simulated node with
`curl -u sim:sim-dev-only -X POST localhost:8443/sim/systems/node-007/tamper -d '{"component":"bootloader"}'`.

## Automated remediation

A fatal GPU XID quarantines the node without a human sending an event:

```
agent (node_id label) -> Prometheus GPUFatalXid -> Alertmanager
  -> POST /v1/alerts (bearer token) -> control plane -> node quarantined
  -> reconciler replaces it for the tenant, if one holds it
```

Alertmanager routes `GPUFatalXid` to the control plane webhook and drops
everything else. Resolved alerts never un-quarantine a node: repair is a human
decision (`repaired` event). At most 5 nodes per 10 minutes are quarantined this
way; beyond that the outcome is `rate_limited` and the `RemediationRateLimited`
alert pages instead, so one noisy rule cannot empty the fleet. Every handled
alert is recorded with its outcome. Contract: [docs/remediation.md](docs/remediation.md).

Demo, with the stack, bmcsim, the seeded fleet and the control plane running
(the compose agents are `node-001` to `node-004`, published on host ports
19401 to 19404):

```bash
curl -X POST localhost:19403/fake/xid -d '{"gpu":3,"code":79}'
```

About a minute and a half later (`for: 1m`, Alertmanager `group_wait: 10s`)
`node-003` is quarantined and the action is recorded:

```bash
curl -s localhost:8080/v1/remediations
```

Clear the fault, as a GPU reset would:

```bash
curl -X DELETE localhost:19403/fake/xid
```

## Develop

```bash
docker compose -f deploy/compose.yaml up -d
```

This starts Postgres, Prometheus, Alertmanager, Grafana and four GPU agents
(`agent-node-001` to `agent-node-004`, one per simulated host `node-001` to
`node-004`), all on the fake GPU backend. The control plane runs on the host.
Start only the observability side with
`docker compose -f deploy/compose.yaml up -d prometheus alertmanager grafana`.
To run the control plane, bmcsim and chaos in containers too, see
[Quick demo](#quick-demo).

Simulated fleet (50 nodes, 5% inspection failures, 1% flaky BMC requests):

```bash
cd controlplane && BMCSIM_USER=sim BMCSIM_PASSWORD=sim-dev-only go run ./cmd/bmcsim
```

Control plane with 4 workers (`PROVISIONER_WORKERS`, 0 disables them). It
needs an attestation signing key, a base64 Ed25519 seed, and the bearer token
Alertmanager sends to the remediation webhook. The dev token is the content of
`deploy/alertmanager/webhook_token`:

```bash
export ATTEST_SIGNING_KEY=$(head -c 32 /dev/urandom | base64)
export REMEDIATION_WEBHOOK_TOKEN=remediation-dev-only
```

```bash
cd controlplane && DATABASE_URL=postgres://fleet:fleet-dev-only@localhost:5432/fleet?sslmode=disable BMC_USER=sim BMC_PASSWORD=sim-dev-only go run ./cmd/controlplane
```

Register the fleet and watch it inspect itself. The seed places 8 nodes per
rack, racks `r1` to `r4` in pod `p1` and the rest in pod `p2` (50 nodes: 32 in
`p1`, 18 in `p2`):

```bash
BMCSIM_USER=sim BMCSIM_PASSWORD=sim-dev-only deploy/seed.sh
```

```bash
curl -s localhost:8080/v1/nodes?state=quarantined
```

Store integration tests need `TEST_DATABASE_URL` (same URL) and are skipped
without it:

```bash
cd controlplane && go test -race ./...
```

```bash
cd agent && cargo test && FAKE_XID=3:79 cargo run
```

An agent run on the host is not scraped: Prometheus discovers the compose
agents only (DNS name `agent`).

Grafana: http://localhost:3000 (anonymous, opens on the GPU Fleet dashboard),
Prometheus: http://localhost:9090.

## Observability

[docs/metrics.md](docs/metrics.md) is the contract for every metric, dashboard
query and alert. Prometheus scrapes the control plane at
`host.docker.internal:8080` and the agents on :9400 (the DNS name `agent`
resolves to all four), and sends alerts to Alertmanager at
http://localhost:9093.

```
deploy/prometheus/prometheus.yml    scrape config
deploy/prometheus/alerts.yml        alert rules (see localhost:9090/alerts)
deploy/prometheus/alerts_test.yml   promtool unit tests for the rules
deploy/alertmanager/                routing config and the dev-only webhook token
deploy/grafana/                     provisioned datasource and the GPU Fleet dashboard
```

No agent injects a fault on its own: see
[Automated remediation](#automated-remediation) to fire `GPUFatalXid`. The
`observability` CI job runs `promtool check config`, `check rules` and
`test rules` from the pinned Prometheus image, `amtool check-config` from the
pinned Alertmanager image, and checks that every dashboard panel queries the
`prometheus` datasource. To run the same checks locally:

```bash
image=$(docker compose -f deploy/compose.yaml config --images | grep '^prom/prometheus:')
promtool() { docker run --rm --entrypoint promtool -v "$PWD/deploy/prometheus:/etc/prometheus:ro" "$image" "$@"; }
promtool check config /etc/prometheus/prometheus.yml
promtool check rules /etc/prometheus/alerts.yml
promtool test rules /etc/prometheus/alerts_test.yml
image=$(docker compose -f deploy/compose.yaml config --images | grep '^prom/alertmanager:')
docker run --rm --entrypoint amtool -v "$PWD/deploy/alertmanager:/etc/alertmanager:ro" "$image" check-config /etc/alertmanager/alertmanager.yml
```

On Windows Git Bash, prefix the `docker run` with `MSYS_NO_PATHCONV=1`.

## Roadmap

1. ~~Postgres-backed node store and lifecycle API~~
2. ~~Simulated Redfish BMC fleet + provisioner workers~~
3. ~~Agent `/metrics` exporter on :9400, Grafana dashboard~~
4. ~~Capacity reservations with topology awareness~~
5. ~~Tenant attestation~~
6. ~~Automated remediation (Alertmanager to quarantine to replacement)~~
7. Kubernetes deployment (kind + kustomize, CI smoke test)
