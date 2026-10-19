# BMC and deploy-agent protocol

The contract between the control plane and machines. `cmd/bmcsim` serves it
for simulated machines; `internal/redfish` and `internal/deployagent` are the
clients. Both sides implement this document independently, so the client
tests against the simulator verify the contract.

A node's `bmc` field is the base URL of its BMC (e.g. `http://localhost:8443`).
The Redfish system ID equals the control-plane node ID.

All endpoints require HTTP basic auth. Errors:

| Status | Meaning | Client error |
|---|---|---|
| 400 | malformed body or unknown enum value | plain error |
| 401 | bad credentials | `ErrUnauthorized` |
| 404 | unknown system | `ErrNotFound` |
| 502, 504 | proxy in front of the BMC could not reach it (retry) | `ErrTransient` |
| 503 | BMC busy or flaky (retry) | `ErrTransient` |

Network errors and timeouts are also `ErrTransient`.

## Redfish subset (out-of-band)

### `GET /redfish/v1/Systems`

```json
{"Members": [{"@odata.id": "/redfish/v1/Systems/node-001"}], "Members@odata.count": 1}
```

### `GET /redfish/v1/Systems/{id}`

```json
{
  "@odata.id": "/redfish/v1/Systems/node-001",
  "Id": "node-001",
  "PowerState": "On",
  "Status": {"State": "Enabled", "Health": "OK"},
  "Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}
}
```

- `PowerState`: `On` | `Off`
- `Status.Health`: `OK` | `Critical`
- `BootSourceOverrideTarget`: `None` | `Pxe`
- `BootSourceOverrideEnabled`: `Disabled` | `Once`

### `PATCH /redfish/v1/Systems/{id}`

```json
{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}
```

Returns 204.

### `POST /redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset`

```json
{"ResetType": "On"}
```

`ResetType`: `On` | `ForceOff` | `ForceRestart`. Returns 204. `On` when
already on is a no-op (idempotent).

## Deploy agent (in-band, simulator-specific)

On real hardware the control plane serves a per-node iPXE script that picks
the ramdisk image, and the ramdisk agent reports results. The simulator
models both with these endpoints on the same base URL.

### `PUT /agent/{id}/task`

```json
{"task": "inspect"}
```

`task`: `inspect` | `wipe` | `deploy`. Selects what the next PXE boot runs.
Returns 204.

### `GET /agent/{id}/status`

```json
{"task": "inspect", "phase": "running"}
```

`phase`: `idle` | `running` | `succeeded` | `failed`. `task` is empty when idle.

## Machine semantics

- Powering on (`On` from Off, or `ForceRestart`) with override `Pxe`/`Once`
  and a task set: the override is consumed (back to `None`/`Disabled`) and
  the agent enters `running` for that task.
- Powering on without a PXE override boots from disk: phase `idle`.
- A running task resolves to `succeeded` or `failed` once boot latency plus
  task duration have elapsed. The outcome is decided once and is sticky.
- `ForceOff` aborts a running task: phase returns to `idle`. A finished
  phase (`succeeded`/`failed`) stays readable until the next power-on.
- A machine with `Health: Critical` fails any running task.
