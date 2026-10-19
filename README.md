# gpu-fleet

A small control plane for bare-metal GPU clouds: node lifecycle, capacity
reservations and GPU health, runnable on a laptop against a simulated fleet.

```
controlplane/   Go: lifecycle state machine, provisioning + capacity APIs
agent/          Rust: per-node GPU health agent (NVML, fake backend for dev)
deploy/         docker compose: Postgres, Prometheus, Grafana
docs/adr/       architecture decision records
```

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
| `POST` | `/v1/nodes` | `{"id","rack","bmc"}`, registers a node in `racked`; `bmc` is the BMC base URL (absolute http/https URL, no credentials, query or fragment) |
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

## Develop

```bash
docker compose -f deploy/compose.yaml up -d
```

Simulated fleet (50 nodes, 5% inspection failures, 1% flaky BMC requests):

```bash
cd controlplane && BMCSIM_USER=sim BMCSIM_PASSWORD=sim-dev-only go run ./cmd/bmcsim
```

Control plane with 4 workers (`PROVISIONER_WORKERS`, 0 disables them):

```bash
cd controlplane && DATABASE_URL=postgres://fleet:fleet-dev-only@localhost:5432/fleet?sslmode=disable BMC_USER=sim BMC_PASSWORD=sim-dev-only go run ./cmd/controlplane
```

Register the fleet and watch it inspect itself:

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

Grafana: http://localhost:3000, Prometheus: http://localhost:9090.

## Roadmap

1. ~~Postgres-backed node store and lifecycle API~~
2. ~~Simulated Redfish BMC fleet + provisioner workers~~
3. Agent `/metrics` exporter on :9400, Grafana dashboard
4. Capacity reservations with topology awareness
5. Stretch: Kubernetes remediation operator or tenant attestation
