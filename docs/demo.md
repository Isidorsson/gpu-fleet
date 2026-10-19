# One-command demo

```bash
docker compose -f deploy/compose.yaml --profile demo up -d --build
```

Starts the whole system in containers and keeps it busy: a simulated 50-node
fleet is racked, inspected and attested, tenants book reservations, a chaos
service plants implants and GPU faults, and the platform catches and repairs
them on its own. Open Grafana at http://localhost:3000.

This document is the contract between the container images, the compose
`demo` profile and the chaos service.

## Images

One Dockerfile, `controlplane/Dockerfile`, build context the repository root,
`ARG CMD` selects the binary (`controlplane`, `bmcsim` or `chaos`). Static Go
build (`CGO_ENABLED=0`), final image `gcr.io/distroless/static-debian12:nonroot`,
entrypoint `/app`. The control plane image also carries
`deploy/attestation/reference.json` at `/etc/gpu-fleet/reference.json`.

## Compose `demo` profile

Services added under `profiles: [demo]` (the default `up` is unchanged):

| Service | Image | Notes |
|---|---|---|
| `controlplane` | `CMD=controlplane` | publishes `8080:8080`, so Prometheus and Alertmanager keep reaching it at `host.docker.internal:8080` unchanged; waits for healthy Postgres |
| `bmcsim` | `CMD=bmcsim` | publishes `8443:8443` for manual `curl` experiments |
| `chaos` | `CMD=chaos` | starts after `controlplane` and `bmcsim` |

Control plane environment in the demo (all values dev-only, like the
Postgres password already in compose):
`DATABASE_URL=postgres://fleet:fleet-dev-only@postgres:5432/fleet?sslmode=disable`,
`BMC_USER=sim`, `BMC_PASSWORD=sim-dev-only`,
`ATTEST_SIGNING_KEY=ZGVtby1vbmx5LWF0dGVzdC1zaWduaW5nLWtleS0wMDE=` (base64 of the
32 bytes `demo-only-attest-signing-key-001`),
`ATTEST_REFERENCE=/etc/gpu-fleet/reference.json`,
`REMEDIATION_WEBHOOK_TOKEN=remediation-dev-only`.

bmcsim: `BMCSIM_USER=sim`, `BMCSIM_PASSWORD=sim-dev-only`, default flags.

Nodes register with `bmc` = `http://bmcsim:8443`, the address the control
plane container reaches.

## Chaos service (`cmd/chaos`, `internal/chaos`)

### Configuration (env)

| Var | Default |
|---|---|
| `CHAOS_CONTROLPLANE_URL` | `http://controlplane:8080` |
| `CHAOS_BMCSIM_URL` | `http://bmcsim:8443` |
| `CHAOS_NODE_BMC_URL` | same as `CHAOS_BMCSIM_URL` (the `bmc` field registered for every node) |
| `BMCSIM_USER`, `BMCSIM_PASSWORD` | required |
| `CHAOS_AGENTS` | `node-001=http://agent-node-001:9400,node-002=...,node-004=http://agent-node-004:9400` |
| `CHAOS_SEED` | `1` |
| `CHAOS_TICK` | `10s` |

### Startup

Wait (with backoff, bounded) for `GET /healthz` on the control plane and the
bmcsim systems list. Register every bmcsim system exactly like
`deploy/seed.sh`: 8 nodes per rack, racks `r1`..`r4` in pod `p1`, the rest in
`p2`, `ak_pub` from `GET /agent/{id}/identity`. A 409 (already registered) is
fine, so restarts are idempotent.

### Each tick

Take a snapshot (`GET /v1/nodes`, `GET /v1/reservations`), let a pure planner
choose at most one action, execute it, log one human-readable line.

| Action | When | Effect |
|---|---|---|
| `reserve` | fewer than 2 reservations `scheduled`/`active`/`degraded` | tenant `tenant-<n>`, policy random of `same_rack`/`same_pod`/`any`, count 4..12, starts now, lasts 3..6 min. A 409 (no capacity) is logged and fine. |
| `implant` | at most every 5 min, an active reservation exists | a node of an active reservation that is not agent-backed gets a random tamper (`firmware`, `bmc`, `bootloader`, `kernel`, `gpu_vbios`, `cc_mode`). It is caught when the reservation ends and the node is wiped and re-attested. |
| `xid` | at most every 4 min, an agent-backed node is `active` | that agent gets XID 79, 48 or 94 on a random GPU via `POST /fake/xid`. Remediation quarantines the node, the reservation gets a replacement. |
| `repair` | a node has been `quarantined` for at least 2 min | clear its tamper (`DELETE /sim/systems/{id}/tamper`), clear its agent's XIDs if agent-backed, then send the `repaired` event. The node goes wipe, inspect, attest and back to `available`. |

Priority when several apply: `repair`, `xid`, `implant`, `reserve`. Never
more than 3 nodes quarantined or faulted-but-not-yet-quarantined at once,
and never more than 3 `xid` actions per 10 minutes (below the remediation
rate limit). Randomness comes from `CHAOS_SEED`, so a run is reproducible
given the same fleet behaviour.

The planner is pure: `Plan(snapshot, state, now, rng) (Action, bool)`. The
executor talks HTTP. Both are unit-tested without a network.
