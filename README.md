# gpu-fleet

[![ci](https://github.com/Isidorsson/gpu-fleet/actions/workflows/ci.yml/badge.svg)](https://github.com/Isidorsson/gpu-fleet/actions/workflows/ci.yml)

A control plane for bare-metal GPU clouds: node lifecycle, topology-aware
capacity reservations, tenant attestation and automated remediation. It runs on
a laptop against a simulated fleet.

**[Project page](https://isidorsson.github.io/gpu-fleet/)** · [Fleet replay](https://isidorsson.github.io/gpu-fleet/#fleet-replay) · [Run it yourself](#run-it-yourself)

## Demo

A chaos service keeps the simulated fleet busy: nodes get implants and GPU
faults, the platform catches them, quarantines the nodes, replaces them for
tenants and repairs them.

- Project page with a walkthrough of the recorded incidents: https://isidorsson.github.io/gpu-fleet/
- Fleet replay, the same chaos rules running in the browser on simulated data,
  with the dashboard panels and a read-only API console: https://isidorsson.github.io/gpu-fleet/#fleet-replay
- The real thing, with Grafana and the full API: [run it yourself](#run-it-yourself) with one Docker Compose command

Once it is running locally:

```bash
curl -s http://localhost:8080/v1/capacity                    # free nodes per pod
curl -s http://localhost:8080/v1/remediations                # automatic quarantines and their outcomes
curl -s http://localhost:8080/v1/attestation/keys            # public key that signs attestation results
curl -s http://localhost:8080/v1/nodes/node-001/attestation  # latest signed result, verifiable offline
```

On Windows PowerShell, `curl` is an alias for `Invoke-WebRequest`: use `curl.exe` instead, or open the URLs in a browser.

It can also be hosted as a read-only showcase: [deploy/railway/README.md](deploy/railway/README.md).

## What it does

- **Provisioning with leases and fencing**: workers claim nodes via `SKIP LOCKED` and drive them through Redfish and an in-band agent. [Details](docs/architecture.md#provisioning-leases-and-fencing)
- **Topology-aware reservations**: `same_rack`, `same_pod` or `any` placement; an exclusion constraint prevents double booking. [Reservations](docs/reservations.md)
- **Tenant attestation**: measured boot and per-GPU reports (simulated) verified after every wipe; signed results, verifiable offline. [Attestation](docs/attestation.md)
- **Automated remediation**: fatal GPU XID, alert, webhook, quarantine, replacement; rate-limited. [Remediation](docs/remediation.md)
- **Observability**: dcgm-exporter compatible metrics, a Grafana dashboard, alert rules with promtool tests. [Metrics](docs/metrics.md)
- **Chaos-driven demo**: one command starts a 50-node simulated fleet that books, breaks and heals itself. [Demo](docs/demo.md)

## Architecture

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

Workers inside the control plane drive simulated BMCs (bmcsim) through Redfish
and an in-band deploy agent. A leader-elected reconciler starts reservations,
swaps in replacements for quarantined nodes and ends them. Chaos also plants
implants through bmcsim and injects GPU faults into the agents. Details:
[docs/architecture.md](docs/architecture.md).

## Run it yourself

The whole system, including the simulated fleet and the chaos service, runs
locally with Docker Compose. You need Docker (Docker Desktop on Windows and
macOS) with Compose v2, and free host ports 3000, 5432, 8080, 8443, 9090, 9093
and 19401 to 19404.

```bash
git clone https://github.com/Isidorsson/gpu-fleet
```

```bash
cd gpu-fleet
```

```bash
docker compose -f deploy/compose.yaml --profile demo up -d --build
```

The first build takes a few minutes. The 50 simulated nodes then need a few
more to pass inspection and attestation, after which chaos starts booking
reservations and injecting faults.

- Grafana, anonymous, opens on the GPU Fleet dashboard: http://localhost:3000
- Prometheus and its alerts: http://localhost:9090
- Alertmanager: http://localhost:9093
- Control plane API (full, not read-only): http://localhost:8080/v1/capacity
- What chaos is doing: `docker compose -f deploy/compose.yaml --profile demo logs -f chaos`

On Windows PowerShell use `curl.exe` rather than `curl`, or open the URLs in a
browser.

Pause it and keep the fleet:

```bash
docker compose -f deploy/compose.yaml --profile demo stop
```

Remove it and its data:

```bash
docker compose -f deploy/compose.yaml --profile demo down -v
```

Without `--profile demo`, Compose starts only Postgres, the agents and the
monitoring stack, for running the control plane from source. More in
[docs/demo.md](docs/demo.md) and [docs/development.md](docs/development.md#running-the-demo).

## Repository layout

```
controlplane/   Go: lifecycle state machine, provisioner, reservations, attestation, remediation, API
  cmd/          controlplane, bmcsim (BMC fleet simulator), chaos, attestref
agent/          Rust: per-node GPU health agent, dcgm-exporter compatible metrics (simulated GPUs)
deploy/         docker compose: Postgres, GPU agents, Prometheus + alerts, Alertmanager, Grafana, demo profile
deploy/railway/ images and configs for the hosted demo
deploy/k8s/     kustomize manifests for the demo on Kubernetes (kind)
site/           project page, deployed to GitHub Pages
docs/           architecture, development guide and component contracts
docs/adr/       architecture decision records
```

## Design notes

- **Pure core, constraints as backstop.** The lifecycle state machine is pure and tested without a database; Postgres `CHECK` constraints repeat its invariants as defense in depth.
- **Concurrency lives in Postgres.** Every event runs under a row lock, workers claim nodes with `SKIP LOCKED` leases and fenced results, and an exclusion constraint on `(node_id, period)` rules out double booking.
- **Fail-closed attestation.** No node reaches `available` without passing attestation, and the reference policy must state `require_cc_mode` explicitly, so it cannot be disabled by omission.
- **Read-only public listener by construction.** The hosted demo serves a second listener that wraps the same API and passes only GET and HEAD; writes live on a private port.
- **Contracts and simulators.** Each component has a contract doc, and bmcsim, the fake GPU backend and chaos let the whole system run without hardware.

## Docs

- [Architecture](docs/architecture.md): components, node lifecycle and invariants, leases and fencing
- [Development](docs/development.md): run it on the host, configuration, tests, observability checks, API reference
- [Demo](docs/demo.md): the `demo` compose profile, images and the chaos service
- [Reservations](docs/reservations.md): topology, placement, schema, reconciler, API
- [Attestation](docs/attestation.md): threat model, signed messages, verification, mapping to real hardware
- [Remediation](docs/remediation.md): alert to quarantine pipeline, rate limit, audit log
- [Metrics](docs/metrics.md): every metric, dashboard query and alert
- [BMC protocol](docs/bmc-protocol.md): the Redfish subset and deploy agent contract
- [ADR 1](docs/adr/0001-go-control-plane-rust-agent.md): Go control plane, Rust agent, one repository
- [Kubernetes](docs/kubernetes.md): the demo on kind with kustomize, and the CI smoke test
- [Hosted deployment](deploy/railway/README.md): Railway services, secrets and configs

## Roadmap

- [x] Postgres-backed node store and lifecycle API
- [x] Simulated Redfish BMC fleet and provisioner workers
- [x] Agent `/metrics` exporter on :9400, Grafana dashboard
- [x] Capacity reservations with topology awareness
- [x] Tenant attestation
- [x] Automated remediation (Alertmanager to quarantine to replacement)
- [x] One-command demo and hosted read-only showcase
- [x] Kubernetes deployment (kind + kustomize, CI smoke test)
