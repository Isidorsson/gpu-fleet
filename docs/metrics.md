# Metrics contract

The single source of truth for every metric gpu-fleet exposes and every
query the dashboard and alerts use. Exporters, dashboard and alert rules are
built against this document independently.

## GPU agent (`agent/`)

Served at `GET /metrics` on `AGENT_LISTEN` (default `0.0.0.0:9400`), the
same port as NVIDIA dcgm-exporter. `GET /healthz` returns 200.

GPU metric names match dcgm-exporter, so the dashboard works unchanged on
real hosts running dcgm-exporter.

### Per-GPU metrics

Labels on every per-GPU metric: `gpu` (index, `"0"`..), `UUID`, `modelName`,
`Hostname`.

| Metric | Type | Unit | Notes |
|---|---|---|---|
| `DCGM_FI_DEV_GPU_TEMP` | gauge | °C | |
| `DCGM_FI_DEV_POWER_USAGE` | gauge | W | |
| `DCGM_FI_DEV_XID_ERRORS` | gauge | code | most recent XID code, `0` when none |
| `DCGM_FI_DEV_ECC_DBE_VOL_TOTAL` | gauge | count | uncorrected (double-bit) ECC errors; a gauge here so the name carries no `_total` suffix, matching dcgm-exporter |
| `gpu_agent_gpu_healthy` | gauge | 0/1 | `1` unless uncorrected ECC > 0 or a fatal XID (see `agent/src/gpu.rs`) |

### Agent self-metrics

Label: `Hostname`.

| Metric | Type | Notes |
|---|---|---|
| `gpu_agent_samples_total` | counter | successful backend samples |
| `gpu_agent_sample_errors_total` | counter | failed backend samples |
| `gpu_agent_last_sample_timestamp_seconds` | gauge | Unix time of the last successful sample |

### Agent configuration (env)

| Var | Default | |
|---|---|---|
| `AGENT_LISTEN` | `0.0.0.0:9400` | |
| `AGENT_SAMPLE_INTERVAL_SECS` | `5` | |
| `AGENT_HOSTNAME` | `$HOSTNAME`, else `localhost` | value of the `Hostname` label |
| `FAKE_GPUS` | `8` | fake backend GPU count |
| `FAKE_XID` | unset | `<gpu>:<code>`, injected XID |

Fake backend values are a deterministic function of (gpu index, time), so
graphs move and tests can pin time: temperature between 40 and 80 °C, power
between 100 and 700 W, model `NVIDIA H100 80GB HBM3`, UUID
`GPU-<hostname>-<index>`.

## Control plane (`controlplane/`)

Served at `GET /metrics` on the API listener (`CONTROLPLANE_ADDR`, default
`:8080`). Go runtime and process collectors are included.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `fleet_nodes` | gauge | `state` | computed at scrape time with one `GROUP BY state` query; all 7 states always present (0 when empty) |
| `provisioner_tasks_total` | counter | `task`, `outcome` | one per task run by a worker |
| `provisioner_task_duration_seconds` | histogram | `task` | task start to result; buckets `1, 5, 15, 30, 60, 120, 300, 600, 1200` |
| `provisioner_bmc_requests_total` | counter | `op`, `result` | every BMC or agent call attempt, retries included |
| `provisioner_events_total` | counter | `event` | lifecycle events applied by workers |
| `provisioner_lease_lost_total` | counter | | sessions abandoned because the lease was lost |

Label values:

- `task`: `inspect` \| `wipe` \| `deploy`
- `outcome`: `succeeded` (agent reported success) \| `failed` (agent reported
  failure) \| `fault` (node quarantined via `hardware_fault`: retries
  exhausted, non-transient error or timeout) \| `abandoned` (lease lost,
  operator changed the node, or shutdown)
- `op`: `set_task` \| `set_pxe` \| `reset` \| `status` \| `attest`
- `result`: `ok` \| `transient` \| `error`
- `event`: any `node.Event` value
- `state`: any `node.State` value

## Capacity (control plane)

Same `/metrics` endpoint. See [reservations.md](reservations.md).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `capacity_free_nodes` | gauge | `pod` | computed at scrape time from `FreeCandidates(now, now+1m)`; one series per pod that has any node |
| `reservations` | gauge | `status` | computed at scrape time with `CountReservationsByStatus`; all 5 statuses always present |
| `placement_failures_total` | counter | `policy` | requests rejected for insufficient capacity; all 3 policies pre-initialized to 0 |
| `reservation_degraded_total` | counter | | reservations that became degraded: started with fewer nodes than requested, or lost a node while running |

## Scrape config

| Job | Targets |
|---|---|
| `controlplane` | `host.docker.internal:8080` (runs on the host during development) |
| `gpu-agent` | `dns_sd_configs` A records for compose services `agent` and `agent-faulty`, port 9400 |

## Dashboard: "GPU Fleet" (uid `gpu-fleet`)

| Panel | Type | Query |
|---|---|---|
| Nodes by state | time series, stacked | `sum by (state) (fleet_nodes)` |
| Quarantined | stat | `sum(fleet_nodes{state="quarantined"})` |
| Unhealthy GPUs | stat | `count(gpu_agent_gpu_healthy == 0) or vector(0)` |
| Quarantine causes (15m) | bar gauge | `sum by (event) (increase(provisioner_events_total{event=~"inspection_failed\|attestation_failed\|wipe_failed\|hardware_fault"}[15m]))` |
| Task duration p50 / p95 | time series | `histogram_quantile(0.5, sum by (task, le) (rate(provisioner_task_duration_seconds_bucket[5m])))` and the same with `0.95` |
| Task outcomes | time series | `sum by (task, outcome) (rate(provisioner_tasks_total[5m]))` |
| BMC error ratio | time series | `sum by (op) (rate(provisioner_bmc_requests_total{result!="ok"}[5m])) / sum by (op) (rate(provisioner_bmc_requests_total[5m]))` |
| GPU temperature | heatmap or time series | `DCGM_FI_DEV_GPU_TEMP` by `Hostname`, `gpu` |
| Power per host | time series | `sum by (Hostname) (DCGM_FI_DEV_POWER_USAGE)` |
| Latest XIDs | table | `DCGM_FI_DEV_XID_ERRORS > 0` (any code; fatality is `gpu_agent_gpu_healthy`) |

Row "Capacity" (between Fleet and Provisioning):

| Panel | Type | Query |
|---|---|---|
| Free nodes by pod | bar gauge | `capacity_free_nodes` by `pod` |
| Reservations by status | time series, stacked | `sum by (status) (reservations)` |
| Placement failures | time series | `sum by (policy) (rate(placement_failures_total[5m]))` |
| Degraded reservations (24h) | stat | `increase(reservation_degraded_total[24h])` |

## Alerts

| Alert | Expr | For | Severity |
|---|---|---|---|
| `GPUFatalXid` | `gpu_agent_gpu_healthy == 0` | 1m | critical |
| `QuarantineSpike` | `sum(increase(provisioner_events_total{event=~"inspection_failed\|attestation_failed\|wipe_failed\|hardware_fault"}[15m])) > 5` | 0m | warning |
| `GPUAgentDown` | `up{job="gpu-agent"} == 0` | 2m | warning |
| `GPUAgentStale` | `time() - gpu_agent_last_sample_timestamp_seconds > 60` | 2m | warning |
| `ControlPlaneDown` | `up{job="controlplane"} == 0` | 2m | critical |
| `ReservationDegraded` | `increase(reservation_degraded_total[15m]) > 0` | 0m | warning |

## Attestation (control plane)

See [attestation.md](attestation.md).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `attestations_total` | counter | `verdict` | one per attestation run by a worker; `pass` and `fail` pre-initialized to 0 |

`attestation_failed` is a quarantine cause: it is pre-initialized in
`provisioner_events_total` and included in the `QuarantineSpike` regex and
the "Quarantine causes" panel.

| Panel (Fleet row) | Type | Query |
|---|---|---|
| Attestation failures (24h) | stat | `sum(increase(attestations_total{verdict="fail"}[24h]))` |

| Alert | Expr | For | Severity |
|---|---|---|---|
| `AttestationFailure` | `increase(attestations_total{verdict="fail"}[15m]) > 0` | 0m | warning |
