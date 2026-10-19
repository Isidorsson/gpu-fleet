# Railway deployment

The live demo runs the same images as the compose demo, one Railway service
each, in a single project.

- **Public:** `controlplane`, through its read-only listener (`PUBLIC_ADDR=:8080`;
  the full API listens on `CONTROLPLANE_ADDR=:8081`, private only), and
  `grafana`, as an anonymous viewer.
- **Private network only:** `bmcsim`, `chaos`, `agent-node-001`,
  `agent-node-002`, `prometheus`, `alertmanager` and `Postgres` (Railway's
  Postgres template).

## Design

- **Read-only by construction.** The control plane serves two listeners from
  one handler: the full API on a port that only the private network reaches
  (chaos and Alertmanager use it), and a copy wrapped in `api.ReadOnly` that
  rejects everything but GET and HEAD on the public port. The public side
  cannot reach a write path whatever a request claims.
- **Secrets** (BMC credentials, attestation signing key, webhook token,
  Grafana admin password) are generated per deployment and stored as Railway
  shared variables; services reference them as `${{shared.NAME}}`. The values
  committed in `deploy/compose.yaml` are local dev-only defaults and are not
  used here.
- **Configs.** Railway has no bind mounts, so Prometheus, Alertmanager and
  Grafana get small images that bake in a Railway variant of their config:
  private DNS names (`*.railway.internal`), IPv6 listeners, and the webhook
  token read from the environment at start-up instead of baked into an image.
- **Deploys** follow `main`: every push rebuilds the services from this
  repository.

## Reproduce it in your own project

1. Create a Postgres database and one empty service per row of the table
   below. Names matter, they are the private DNS names.
2. Set each service's Dockerfile path, and root directory `agent` or `deploy`
   for the agent and monitoring images.
3. Set the shared variables `BMC_USER`, `BMC_PASSWORD`, `ATTEST_SIGNING_KEY`
   (base64 of 32 random bytes), `REMEDIATION_WEBHOOK_TOKEN` and
   `GRAFANA_ADMIN_PASSWORD`, and the per-service variables in the table.
4. Connect the repository, and generate domains for `controlplane` (port 8080,
   the read-only listener) and `grafana` (port 3000).

## Per-service settings

Shared variables (set once on the environment, generated per deployment):
`BMC_USER`, `BMC_PASSWORD`, `ATTEST_SIGNING_KEY` (base64 of 32 random bytes),
`REMEDIATION_WEBHOOK_TOKEN`, `GRAFANA_ADMIN_PASSWORD`.

| Service | Root dir | Dockerfile | Variables |
|---|---|---|---|
| `controlplane` | `/` | `controlplane/Dockerfile` | `CMD=controlplane`, `DATABASE_URL=${{Postgres.DATABASE_URL}}`, `CONTROLPLANE_ADDR=:8081`, `PUBLIC_ADDR=:8080`, `PORT=8080`, `BMC_USER=${{shared.BMC_USER}}`, `BMC_PASSWORD=${{shared.BMC_PASSWORD}}`, `ATTEST_SIGNING_KEY=${{shared.ATTEST_SIGNING_KEY}}`, `ATTEST_REFERENCE=/etc/gpu-fleet/reference.json`, `REMEDIATION_WEBHOOK_TOKEN=${{shared.REMEDIATION_WEBHOOK_TOKEN}}` |
| `bmcsim` | `/` | `controlplane/Dockerfile` | `CMD=bmcsim`, `BMCSIM_USER=${{shared.BMC_USER}}`, `BMCSIM_PASSWORD=${{shared.BMC_PASSWORD}}` |
| `chaos` | `/` | `controlplane/Dockerfile` | `CMD=chaos`, `CHAOS_CONTROLPLANE_URL=http://controlplane.railway.internal:8081`, `CHAOS_BMCSIM_URL=http://bmcsim.railway.internal:8443`, `CHAOS_AGENTS=node-001=http://agent-node-001.railway.internal:9400,node-002=http://agent-node-002.railway.internal:9400`, `BMCSIM_USER=${{shared.BMC_USER}}`, `BMCSIM_PASSWORD=${{shared.BMC_PASSWORD}}` |
| `agent-node-001`, `agent-node-002` | `/agent` | `Dockerfile` | `AGENT_NODE_ID` and `AGENT_HOSTNAME` = `node-001` / `node-002`, `AGENT_LISTEN=[::]:9400` |
| `prometheus` | `/deploy` | `railway/prometheus.Dockerfile` | none |
| `alertmanager` | `/deploy` | `railway/alertmanager.Dockerfile` | `REMEDIATION_WEBHOOK_TOKEN=${{shared.REMEDIATION_WEBHOOK_TOKEN}}` |
| `grafana` | `/deploy` | `railway/grafana.Dockerfile` | `GF_SECURITY_ADMIN_PASSWORD=${{shared.GRAFANA_ADMIN_PASSWORD}}`, `PORT=3000` |

Public domains: `controlplane` on port 8080 (the read-only listener) and
`grafana` on port 3000. Everything else stays on the private network.

Nodes are registered by chaos with `bmc=http://bmcsim.railway.internal:8443`
(`CHAOS_NODE_BMC_URL` defaults to `CHAOS_BMCSIM_URL`). Listeners use `[::]` or
`:port` because Railway's private network may be IPv6-only.

## Watch paths and health checks

Each service redeploys only when its own sources change, so docs and project
page commits do not restart anything:

| Service | Watch paths | Health check |
|---|---|---|
| `controlplane` | `/controlplane/**`, `/deploy/attestation/**` | `/healthz` |
| `bmcsim` | `/controlplane/**`, `/deploy/attestation/**` | |
| `chaos` | `/controlplane/**` | |
| `agent-node-001`, `agent-node-002` | `/agent/**` | |
| `prometheus` | `/deploy/railway/**`, `/deploy/prometheus/**` | |
| `alertmanager` | `/deploy/railway/**` | |
| `grafana` | `/deploy/railway/**`, `/deploy/grafana/**` | `/api/health` |

The two public services have health checks so Railway keeps routing to the
old container until the new one answers, which makes their deploys
zero-downtime.
