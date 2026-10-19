# gpu-fleet

A small control plane for bare-metal GPU clouds: node lifecycle, capacity
reservations and GPU health, runnable on a laptop against a simulated fleet.

```
controlplane/   Go: lifecycle state machine, provisioning + capacity APIs
agent/          Rust: per-node GPU health agent (NVML, fake backend for dev)
deploy/         docker compose: Postgres, GPU agents, Prometheus + alerts, Grafana
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

This starts Postgres, Prometheus, Grafana and the GPU agents (3 `agent`
replicas plus `agent-faulty`, which injects XID 79 on GPU 3), all on the fake
GPU backend. The control plane runs on the host. Start only the observability
side with `docker compose -f deploy/compose.yaml up -d prometheus grafana`.

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

An agent run on the host is not scraped: Prometheus discovers the compose
`agent` services only.

Grafana: http://localhost:3000 (anonymous, opens on the GPU Fleet dashboard),
Prometheus: http://localhost:9090.

## Observability

[docs/metrics.md](docs/metrics.md) is the contract for every metric, dashboard
query and alert. Prometheus scrapes the control plane at
`host.docker.internal:8080` and the agents on :9400 (DNS A records for the
`agent` and `agent-faulty` services).

```
deploy/prometheus/prometheus.yml    scrape config
deploy/prometheus/alerts.yml        alert rules (see localhost:9090/alerts)
deploy/prometheus/alerts_test.yml   promtool unit tests for the rules
deploy/grafana/                     provisioned datasource and the GPU Fleet dashboard
```

`agent-faulty` makes `GPUFatalXid` fire about a minute after startup. The
`observability` CI job runs `promtool check config`, `check rules` and
`test rules` from the pinned Prometheus image, and checks that every dashboard
panel queries the `prometheus` datasource. To run the same checks locally:

```bash
image=$(docker compose -f deploy/compose.yaml config --images | grep '^prom/prometheus:')
promtool() { docker run --rm --entrypoint promtool -v "$PWD/deploy/prometheus:/etc/prometheus:ro" "$image" "$@"; }
promtool check config /etc/prometheus/prometheus.yml
promtool check rules /etc/prometheus/alerts.yml
promtool test rules /etc/prometheus/alerts_test.yml
```

On Windows Git Bash, prefix the `docker run` with `MSYS_NO_PATHCONV=1`.

## Roadmap

1. ~~Postgres-backed node store and lifecycle API~~
2. ~~Simulated Redfish BMC fleet + provisioner workers~~
3. ~~Agent `/metrics` exporter on :9400, Grafana dashboard~~
4. Capacity reservations with topology awareness
5. Stretch: Kubernetes remediation operator or tenant attestation
