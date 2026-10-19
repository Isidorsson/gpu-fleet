# Capacity reservations

Customers reserve N whole nodes for a time window, placed close together on
the fabric. This document is the contract between the placement package, the
store, the reconciler, the API and the metrics. Each is built independently
against it.

## Topology

```
site -> pod (InfiniBand scalable unit, non-blocking inside) -> rack -> node
```

`nodes.pod` and `nodes.rack` are set at registration. Inside a pod any two
nodes have full bisection bandwidth; across pods traffic crosses the core.
On rack-scale NVLink systems (GB200 NVL72) the NVLink domain is the rack.

## Placement (`internal/placement`, pure)

`func Place(req Request, free []Candidate) ([]string, error)` matches
`placement.Placer`. `free` contains only nodes free for the whole window, in
any order. The result is node IDs sorted ascending. Same input, same output.

`req.Count < 1` or an unknown policy: `ErrInvalidRequest`.

| Policy | Rule |
|---|---|
| `same_rack` | All nodes in one rack. Choose the rack with the fewest free nodes that still has `>= Count` (best fit). Ties: lowest `(pod, rack)`. |
| `same_pod` | All nodes in one pod. Choose the pod with the fewest free nodes that still has `>= Count` (best fit). Ties: lowest pod. Inside the pod: if one rack fits, use the best-fit rack as for `same_rack`; otherwise take racks in ascending free count (ties: lowest rack), consuming each fully before the next, so large racks stay whole. |
| `any` | If some pod fits, exactly as `same_pod`. Otherwise span pods in descending free count (ties: lowest pod), each pod consumed with the `same_pod` inner rule, using as few pods as possible. |

Within a chosen rack, nodes are taken in ascending ID order.

When nothing fits, return `*CapacityError{Policy, Requested, Largest}` where
`Largest` is the largest free rack (`same_rack`), the largest free pod
(`same_pod`), or the total free (`any`).

`func Summarize(free []Candidate) []PodCapacity` returns one entry per pod
present in `free`, sorted by pod, with its free count and its largest free
rack.

## Schema (in `00001_baseline.sql`; pre-launch, single baseline)

- `nodes.pod text NOT NULL`.
- `CREATE EXTENSION IF NOT EXISTS btree_gist;`
- `reservations`: `id text PK` (`res-` + 12 random hex), `tenant text NOT NULL`,
  `node_count int NOT NULL CHECK (node_count BETWEEN 1 AND 64)`,
  `policy text NOT NULL CHECK (policy IN ('same_rack','same_pod','any'))`,
  `period tstzrange NOT NULL CHECK (NOT isempty(period))`,
  `status text NOT NULL CHECK (status IN ('scheduled','active','degraded','ended','cancelled'))`,
  `created_at timestamptz NOT NULL DEFAULT now()`.
- `reservation_nodes`: `reservation_id text REFERENCES reservations ON DELETE CASCADE`,
  `node_id text REFERENCES nodes`, `period tstzrange NOT NULL`,
  `PRIMARY KEY (reservation_id, node_id)`, and
  `EXCLUDE USING gist (node_id WITH =, period WITH &&)`.

Periods are half-open `[start, end)`, so back-to-back reservations on one
node do not overlap.

## Eligibility: which nodes are free for a window

A node is a candidate for `[start, end)` when both hold:

1. Its state is `racked`, `inspecting`, `available` or `wiping`, **or** it is
   `provisioning`/`active` and has a `reservation_nodes` row covering the
   current time (its future is governed by that reservation's period).
   Quarantined nodes and nodes a tenant holds outside any reservation are
   never candidates.
2. It has no `reservation_nodes` row whose period overlaps `[start, end)`.

## Store (`*store.Postgres`): exact signatures

All times are passed in by the caller (the reconciler's clock); reservation
logic does not use the database clock.

```go
func (p *Postgres) FreeCandidates(ctx context.Context, start, end time.Time) ([]placement.Candidate, error)

// CreateReservation runs in one transaction: free candidates for the window,
// place, insert reservation (status scheduled) and its node rows. An
// exclusion violation (SQLSTATE 23P01, another request won the race) retries
// the whole transaction, up to maxPlacementAttempts = 5. A *CapacityError
// from place is returned unchanged (errors.Is ErrInsufficientCapacity).
func (p *Postgres) CreateReservation(ctx context.Context, req reservation.Request, place placement.Placer) (reservation.Reservation, error)

func (p *Postgres) GetReservation(ctx context.Context, id string) (reservation.Reservation, error)       // reservation.ErrNotFound
func (p *Postgres) ListReservations(ctx context.Context, tenant string) ([]reservation.Reservation, error) // "" = all; newest first

// CancelReservation at time now: scheduled -> cancelled and its node rows are
// deleted. active/degraded -> ends_at and node row periods are truncated to
// now (status unchanged; the reconciler ends it). ended/cancelled ->
// reservation.ErrNotCancellable.
func (p *Postgres) CancelReservation(ctx context.Context, id string, now time.Time) (reservation.Reservation, error)

func (p *Postgres) DueToStart(ctx context.Context, now time.Time) ([]reservation.Reservation, error) // scheduled, starts_at <= now
func (p *Postgres) DueToEnd(ctx context.Context, now time.Time) ([]reservation.Reservation, error)   // active|degraded, ends_at <= now
func (p *Postgres) SetReservationStatus(ctx context.Context, id string, s reservation.Status) error

// ReplaceNode swaps oldNodeID for one free candidate for the reservation's
// remaining window [max(now, start), end), restricted to the pod (same_pod)
// or rack (same_rack) of the reservation's other nodes; any: unrestricted.
// Uses place with Count 1. Returns the new node ID or a *CapacityError.
func (p *Postgres) ReplaceNode(ctx context.Context, id, oldNodeID string, now time.Time, place placement.Placer) (string, error)

func (p *Postgres) CountReservationsByStatus(ctx context.Context) (map[reservation.Status]int, error)
```

## Reconciler (`internal/reservation`)

One leader at a time: it holds `pg_try_advisory_lock(<named constant>)` on a
dedicated connection; non-leaders retry every tick. Each tick (default 5s),
with an injected clock:

**Start** every reservation from `DueToStart(now)`. For each of its nodes:
- `quarantined`: `ReplaceNode`. On `*CapacityError` the node is dropped, the
  reservation will be `degraded`, and `Metrics.Degraded()` is called once.
- `available`: apply `provision` with the reservation's tenant (operator-style
  `ApplyEvent`; workers pick it up).
- `provisioning`/`active` with the same tenant: already started, skip.
- anything else (`racked`, `inspecting`, `wiping`, or held by another
  tenant): not ready; leave the reservation `scheduled` and retry next tick.

When every remaining node has been provisioned, set status `active` (or
`degraded` if any node was dropped).

**End** every reservation from `DueToEnd(now)`: apply `release` to each of
its nodes that is `active` with its tenant. A node still `provisioning`
waits for the next tick. When none of its nodes is `provisioning`/`active`
for its tenant any more, set status `ended`.

Every step is idempotent: a crashed tick is simply redone by the next one.

## API

| Method | Path | Body / query | Responses |
|---|---|---|---|
| `POST` | `/v1/reservations` | `{"tenant","count","policy","starts_at","ends_at"}` (RFC 3339) | 201 reservation; 409 `{"error", "largest"}` on insufficient capacity; 422 invalid |
| `GET` | `/v1/reservations/{id}` | | 200; 404 |
| `GET` | `/v1/reservations?tenant=` | | 200 `{"reservations": [...]}` |
| `DELETE` | `/v1/reservations/{id}` | | 200 reservation after cancel; 404; 409 not cancellable |
| `GET` | `/v1/capacity` | `starts_at`, `ends_at` (default now, now+1h) | 200 `{"pods": [{"pod","free","largest_rack"}], "total_free"}` |

Reservation JSON: `id, tenant, count, policy, starts_at, ends_at, status,
nodes, created_at`. `POST /v1/nodes` also requires `pod` (same rules as
`rack`: non-empty).

## Metrics (added to docs/metrics.md)

See the "Capacity" section of [metrics.md](metrics.md).
