# Automated remediation

Closes the loop between GPU health and the node lifecycle. A GPU agent
reports a fatal XID, Prometheus fires `GPUFatalXid`, Alertmanager calls the
control plane's webhook, and the control plane quarantines the node. If a
tenant holds it, the reservation reconciler marks the reservation `degraded`
and provisions a replacement (docs/reservations.md, mid-run health). No
human sends an event.

This document is the contract between the GPU agent, the deploy config, the
remediation package, the store and the API.

```
gpu-agent --(node_id label)--> Prometheus --GPUFatalXid--> Alertmanager
   --POST /v1/alerts (bearer token)--> remediation.Service
   --> store.ApplyRemediation (rate limit + hardware_fault + audit row)
   --> node quarantined --> reconciler replaces it for the tenant
```

## Agent identity (`agent/`)

`AGENT_NODE_ID` (optional, default empty) is the control-plane node ID of the
host the agent runs on. Every metric the agent exports (per-GPU and self
metrics) carries a `node_id` label with that value. Prometheus drops labels
with empty values, so an agent without a node ID simply has no `node_id`.

### Fake-backend control (simulator only)

On the same listener as `/metrics`:

- `POST /fake/xid` body `{"gpu": 3, "code": 79}`: from the next sample on,
  that GPU reports the XID (appended to its history). 204. Bad body or GPU
  out of range: 400.
- `DELETE /fake/xid`: clears all injected XIDs, like a GPU reset. 204.

`FAKE_XID` still sets an XID at startup.

## Deploy (`deploy/`)

- Compose runs four agents, one per host `node-001` to `node-004`
  (`AGENT_NODE_ID` and `AGENT_HOSTNAME` set per service), all sharing the
  network alias `agent`. Prometheus discovers them with one `dns_sd_configs`
  name, `agent`. No agent injects an XID at startup any more.
- Alertmanager `prom/alertmanager:v0.34.1`, `deploy/alertmanager/alertmanager.yml`:
  route grouped by `[alertname, node_id]`, `group_wait: 10s`,
  `group_interval: 1m`, `repeat_interval: 1h`; alerts with
  `alertname="GPUFatalXid"` go to receiver `remediation`, everything else to
  receiver `null`. Receiver `remediation` is a webhook to
  `http://host.docker.internal:8080/v1/alerts` with
  `http_config.authorization.credentials_file: /etc/alertmanager/webhook_token`
  and `send_resolved: true`.
- `deploy/alertmanager/webhook_token` holds the dev-only token
  `remediation-dev-only` (like the dev Postgres password in compose).
- Prometheus sends alerts to `alertmanager:9093`.

## Webhook (`POST /v1/alerts`)

Alertmanager's webhook payload, version 4:

```json
{"version": "4", "status": "firing", "alerts": [
  {"status": "firing", "labels": {"alertname": "GPUFatalXid", "node_id": "node-003", "gpu": "3"},
   "annotations": {}, "startsAt": "2026-10-19T12:00:00Z", "endsAt": "0001-01-01T00:00:00Z",
   "fingerprint": "8f2a..."}]}
```

- Auth: `Authorization: Bearer <token>`, compared in constant time with
  `REMEDIATION_WEBHOOK_TOKEN` (required at control-plane startup). Missing or
  wrong: 401.
- Body limit 1 MiB; malformed JSON or unsupported version: 400.
- 200 with the summary `{"outcomes": {"quarantined": 1, ...}}` after every
  alert is handled. A store error: 500, so Alertmanager retries.

## Policy (`internal/remediation`, pure)

`func Decide(a Alert) (Decision, Outcome)`: alert to decision, no I/O.

| Alert | Outcome |
|---|---|
| `status` is not `firing` | `ignored` (no automatic un-quarantine: repairs are a human decision) |
| `alertname` has no rule | `ignored` |
| no `node_id` label | `ignored` |
| `GPUFatalXid` with `node_id` | decision: action `quarantine` for that node |

Rules are a table (`alertname -> action`) with one entry today.

## Store (`*store.Postgres`): exact signature

```go
// ApplyRemediation, in one transaction under a transaction-scoped advisory
// lock (so concurrent webhooks and replicas share one limit): if the node is
// missing -> OutcomeUnknownNode; if it is already quarantined ->
// OutcomeAlreadyQuarantined; if it is racked (not powered) -> OutcomeNotApplicable;
// if at least limit.Max nodes were quarantined by remediation within
// limit.Window before now -> OutcomeRateLimited; otherwise apply hardware_fault
// (operator-style, no lease) -> OutcomeQuarantined. Every outcome except a
// returned error is recorded in remediation_actions.
func (p *Postgres) ApplyRemediation(ctx context.Context, d remediation.Decision, limit remediation.Limit, now time.Time) (remediation.Outcome, error)

func (p *Postgres) ListRemediations(ctx context.Context, max int) ([]remediation.Record, error) // newest first
```

Table `remediation_actions (id bigint identity PK, node_id text NOT NULL,
alertname text NOT NULL, fingerprint text NOT NULL, action text NOT NULL,
outcome text NOT NULL CHECK (...), at timestamptz NOT NULL)`, index on
`(outcome, at)`. `node_id` is not a foreign key: unknown nodes are recorded
too.

Default limit: at most 5 remediation quarantines per 10 minutes. Above that
the platform stops taking nodes out on its own and the `RemediationRateLimited`
alert pages a human: one noisy rule must not empty the fleet.

## Service

`func (s *Service) Handle(ctx context.Context, w Webhook) (Summary, error)`:
for each alert, `Decide`; if there is a decision, `ApplyRemediation` with the
configured limit and the injected clock's now; count outcomes; report each
outcome to `Metrics.Remediated`. Stops and returns the first store error.

## API

| Method | Path | |
|---|---|---|
| `POST` | `/v1/alerts` | the webhook above |
| `GET` | `/v1/remediations?limit=50` | `{"remediations": [{"node_id","alertname","fingerprint","action","outcome","at"}]}`, newest first; `limit` 1..500, default 50 |

## Metrics and alerts

| Metric | Type | Labels |
|---|---|---|
| `remediation_actions_total` | counter | `outcome`, every outcome pre-initialized to 0 |

| Alert | Expr | For | Severity |
|---|---|---|---|
| `RemediationRateLimited` | `increase(remediation_actions_total{outcome="rate_limited"}[15m]) > 0` | 0m | critical |

`GPUFatalXid` gains `node_id` in its summary. Dashboard (Fleet row): stat
"Auto-quarantined (24h)" = `sum(increase(remediation_actions_total{outcome="quarantined"}[24h]))`.
