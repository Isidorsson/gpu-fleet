# Development

Everything needed to run the pieces by hand, configure them, test them and use
the API. For the architecture see [architecture.md](architecture.md); for the
one-command demo see [Running the demo](#running-the-demo).

## Running the demo

```bash
docker compose -f deploy/compose.yaml --profile demo up -d --build
```

Needs Docker with Compose v2 and free host ports 3000, 5432, 8080, 8443, 9090,
9093 and 19401 to 19404 (stop a host-run control plane first, it also uses
8080). The first run builds the Go and Rust images, which takes a few minutes.

It starts everything in containers and keeps it busy: a simulated 50-node
fleet is racked, inspected and attested (a few minutes), tenants book
reservations, and a chaos service plants implants and GPU faults that the
platform catches and repairs on its own. Contract: [demo.md](demo.md).

- Grafana, anonymous, opens on the GPU Fleet dashboard: http://localhost:3000
- Prometheus and its alerts: http://localhost:9090
- Alertmanager: http://localhost:9093
- Control plane API: `curl -s localhost:8080/v1/capacity`
- What the chaos service is doing:
  `docker compose -f deploy/compose.yaml --profile demo logs -f chaos`

Stop it with `docker compose -f deploy/compose.yaml --profile demo down -v`.
The fleet is stored in the `pgdata` volume while bmcsim keeps its state in
memory, so drop the volume (`-v`) to start the next run clean. The
`demo-smoke` CI job runs the `up` command and waits until 40 nodes have passed
inspection and attestation (`available`, `provisioning` or `active`).

## Run the pieces on the host

```bash
docker compose -f deploy/compose.yaml up -d
```

This starts Postgres, Prometheus, Alertmanager, Grafana and four GPU agents
(`agent-node-001` to `agent-node-004`, one per simulated host `node-001` to
`node-004`), all on the fake GPU backend. The control plane runs on the host.
Start only the observability side with
`docker compose -f deploy/compose.yaml up -d prometheus alertmanager grafana`.
To run the control plane, bmcsim and chaos in containers too, see
[Running the demo](#running-the-demo).

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

Run a GPU agent on the host with an injected XID:

```bash
cd agent && FAKE_XID=3:79 cargo run
```

An agent run on the host is not scraped: Prometheus discovers the compose
agents only (DNS name `agent`).

Grafana: http://localhost:3000 (anonymous, opens on the GPU Fleet dashboard),
Prometheus: http://localhost:9090.

## Configuration

Control plane (`controlplane/cmd/controlplane`):

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | none | required; migrations run at start-up |
| `BMC_USER`, `BMC_PASSWORD` | none | basic-auth credentials for every BMC; required while workers are enabled |
| `PROVISIONER_WORKERS` | `4` | `0` disables the workers |
| `ATTEST_SIGNING_KEY` | none | required; base64 of a 32-byte Ed25519 seed |
| `ATTEST_REFERENCE` | `../deploy/attestation/reference.json` | reference values; the default assumes the working directory is `controlplane/` |
| `REMEDIATION_WEBHOOK_TOKEN` | none | required; bearer token for `POST /v1/alerts` |
| `CONTROLPLANE_ADDR` | `:8080` | the full API and `/metrics` |
| `PUBLIC_ADDR` | unset | when set, a second listener serves the same API read-only (GET and HEAD) |

bmcsim (`controlplane/cmd/bmcsim`): `BMCSIM_USER` and `BMCSIM_PASSWORD` are
required. Flags: `-addr` (`:8443`), `-nodes` (50), `-seed` (42), `-boot-latency`
(3s), `-task-duration` (5s), `-inspect-fail` (0.05), `-wipe-fail` (0.02),
`-deploy-fail` (0.02), `-bmc-error-rate` (0.01, requests answered with 503).

attestref (`controlplane/cmd/attestref`): flags `-seed` and `-version`, both
required. Prints reference values that accept an untampered bmcsim started with
the same seed, the format of `deploy/attestation/reference.json`.

GPU agent: `AGENT_LISTEN`, `AGENT_SAMPLE_INTERVAL_SECS`, `AGENT_HOSTNAME`,
`AGENT_NODE_ID`, `FAKE_GPUS` and `FAKE_XID`, listed in
[metrics.md](metrics.md#agent-configuration-env) and
[remediation.md](remediation.md#agent-identity-agent). Chaos service: see
[demo.md](demo.md#configuration-env).

## Tests

```bash
cd controlplane && go test -race ./...
```

Store and reconciler integration tests need `TEST_DATABASE_URL` (the same URL
as `DATABASE_URL`) and are skipped without it. Each store test creates its own
schema and drops it afterwards.

```bash
cd agent && cargo test
```

CI (`.github/workflows/ci.yml`) runs four jobs:

- `controlplane`: `gofmt`, `go vet` and `go test -race ./...` against a
  Postgres 17.6 service.
- `agent`: `cargo fmt --check`, `cargo clippy --all-targets -- -D warnings` and
  `cargo test`.
- `observability`: see [Observability](#observability).
- `demo-smoke`: starts the demo profile and waits for the fleet to pass
  inspection and attestation, see [Running the demo](#running-the-demo).

## Observability

[metrics.md](metrics.md) is the contract for every metric, dashboard query and
alert. Prometheus scrapes the control plane at `host.docker.internal:8080` and
the agents on :9400 (the DNS name `agent` resolves to all four), and sends
alerts to Alertmanager at http://localhost:9093.

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

## Trying the features by hand

These need the stack, bmcsim, the seeded fleet and the control plane running,
as in [Run the pieces on the host](#run-the-pieces-on-the-host) (or the demo
profile).

### Capacity reservations

Reserve whole nodes for a time window, placed close together on the fabric:
`same_rack`, `same_pod` or `any`. A `starts_at` in the past means now. If the
fleet cannot satisfy the request the API answers 409 with the `largest` block
available. Rules and contract: [reservations.md](reservations.md).

```bash
curl -s -X POST localhost:8080/v1/reservations -H 'Content-Type: application/json' \
  -d '{"tenant":"acme","count":8,"policy":"same_rack","starts_at":"2026-11-01T09:00:00Z","ends_at":"2026-11-01T17:00:00Z"}'
```

```bash
curl -s localhost:8080/v1/capacity
```

### Tenant attestation

Fetch the latest signed result for a node and the public key that verifies it:

```bash
curl -s localhost:8080/v1/nodes/node-001/attestation
```

```bash
curl -s localhost:8080/v1/attestation/keys
```

Inject an implant into a simulated node. It is caught the next time the node
is attested, during its next inspection (the first one after racking, or the
one after its next wipe), and the node moves to `quarantined`
(`attestation_failed`):

```bash
curl -u sim:sim-dev-only -X POST localhost:8443/sim/systems/node-007/tamper -d '{"component":"bootloader"}'
```

The TPM and GPU are simulated in software; [attestation.md](attestation.md)
has the threat model and maps every simulated piece to TPM 2.0, NVIDIA and
IETF RATS equivalents.

### Automated remediation

With the compose agents `node-001` to `node-004` published on host ports 19401
to 19404, inject a fatal XID on GPU 3 of `node-003`:

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

Resolved alerts never un-quarantine a node: send the `repaired` event
yourself. Contract: [remediation.md](remediation.md).

## API

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` | 200 when the process is up |
| `GET` | `/metrics` | Prometheus metrics, see [metrics.md](metrics.md) |
| `POST` | `/v1/nodes` | `{"id","pod","rack","bmc","ak_pub"}`, registers a node in `racked`; `bmc` is the BMC base URL (absolute http/https URL, no credentials, query or fragment); `ak_pub` is the standard base64 of a 32-byte Ed25519 public key |
| `GET` | `/v1/nodes?state=available` | `state` filter is optional |
| `GET` | `/v1/nodes/{id}` | |
| `POST` | `/v1/nodes/{id}/events` | `{"event":"provision","tenant":"acme"}` |
| `GET` | `/v1/nodes/{id}/events` | audit log, oldest first |
| `GET` | `/v1/nodes/{id}/attestation` | latest signed attestation result, see [attestation.md](attestation.md) |
| `GET` | `/v1/attestation/keys` | public key that verifies attestation results |
| `POST` | `/v1/reservations` | `{"tenant","count","policy","starts_at","ends_at"}`, see [reservations.md](reservations.md) |
| `GET` | `/v1/reservations?tenant=` | `tenant` filter is optional |
| `GET` | `/v1/reservations/{id}` | |
| `DELETE` | `/v1/reservations/{id}` | cancels; returns the reservation |
| `GET` | `/v1/capacity` | free nodes per pod for `starts_at` to `ends_at` (default now to now+1h) |
| `POST` | `/v1/alerts` | Alertmanager webhook, bearer token, see [remediation.md](remediation.md) |
| `GET` | `/v1/remediations?limit=50` | remediation audit log, newest first; `limit` 1 to 500 |

Errors are JSON, `{"error": "..."}`:

| Status | Meaning |
|---|---|
| 400 | malformed request: bad JSON, unknown field, body over 1 MiB, bad query value or timestamp, unsupported webhook version |
| 401 | missing or wrong bearer token on `POST /v1/alerts` |
| 404 | unknown node or reservation, or no attestation result for the node yet |
| 409 | invalid transition, duplicate node, reservation not cancellable, or insufficient capacity (the body also carries `largest`) |
| 422 | unknown event or tenant rule violated (`provision` needs a tenant, other events must not have one), invalid node fields, invalid reservation request |
| 503 | a reservation lost the race for its nodes on every placement attempt; retry |
| 500 | internal error |

On the public read-only listener (`PUBLIC_ADDR`) any method other than GET and
HEAD is answered with 405.

Each event runs in one transaction holding a row lock on the node, so
concurrent events on the same node are serialized and the audit log always
matches the node's state. See [Node lifecycle](architecture.md#node-lifecycle)
for the valid transitions.

## Hosted deployment

The live demo runs the same images on Railway. Service layout, the read-only
listener, secrets and reproduction steps: [deploy/railway/README.md](../deploy/railway/README.md).
