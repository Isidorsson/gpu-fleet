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

## Develop

```bash
cd controlplane && go test ./... && go run ./cmd/controlplane
```

```bash
cd agent && cargo test && FAKE_XID=3:79 cargo run
```

```bash
docker compose -f deploy/compose.yaml up -d
```

Grafana: http://localhost:3000, Prometheus: http://localhost:9090.

## Roadmap

1. Postgres-backed node store and lifecycle API
2. Simulated Redfish BMC fleet + provisioner workers
3. Agent `/metrics` exporter on :9400, Grafana dashboard
4. Capacity reservations with topology awareness
5. Stretch: Kubernetes remediation operator or tenant attestation
