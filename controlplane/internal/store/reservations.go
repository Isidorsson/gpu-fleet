package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

var allStatuses = []reservation.Status{
	reservation.StatusScheduled, reservation.StatusActive, reservation.StatusDegraded,
	reservation.StatusEnded, reservation.StatusCancelled,
}

const (
	selectReservation = `SELECT r.id, r.tenant, r.node_count, r.policy, lower(r.period), upper(r.period),
		r.status, r.created_at,
		coalesce((SELECT array_agg(rn.node_id) FROM reservation_nodes rn WHERE rn.reservation_id = r.id), '{}')
		FROM reservations r`
	reservationByID = selectReservation + ` WHERE r.id = $1`
	// lockReservation serializes concurrent cancels and node replacements.
	lockReservation = reservationByID + ` FOR UPDATE OF r`

	dueToStart = selectReservation + ` WHERE r.status = 'scheduled' AND lower(r.period) <= $1
		ORDER BY lower(r.period), r.id`
	running = selectReservation + ` WHERE r.status IN ('active', 'degraded') AND r.period @> $1::timestamptz
		ORDER BY upper(r.period), r.id`
	dueToEnd = selectReservation + ` WHERE r.status IN ('active', 'degraded') AND upper(r.period) <= $1
		ORDER BY upper(r.period), r.id`
)

func scanReservation(row pgx.CollectableRow) (reservation.Reservation, error) {
	var r reservation.Reservation
	var policy, status string
	err := row.Scan(&r.ID, &r.Tenant, &r.Count, &policy, &r.StartsAt, &r.EndsAt, &status, &r.CreatedAt, &r.NodeIDs)
	r.Policy, r.Status = placement.Policy(policy), reservation.Status(status)
	r.StartsAt, r.EndsAt, r.CreatedAt = r.StartsAt.UTC(), r.EndsAt.UTC(), r.CreatedAt.UTC()
	slices.Sort(r.NodeIDs)
	return r, err
}

func getReservation(ctx context.Context, q querier, sql, id string) (reservation.Reservation, error) {
	var zero reservation.Reservation
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return zero, fmt.Errorf("get reservation %s: %w", id, err)
	}
	res, err := pgx.CollectExactlyOneRow(rows, scanReservation)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, fmt.Errorf("%w: %s", reservation.ErrNotFound, id)
	}
	if err != nil {
		return zero, fmt.Errorf("scan reservation %s: %w", id, err)
	}
	return res, nil
}

func (p *Postgres) GetReservation(ctx context.Context, id string) (reservation.Reservation, error) {
	return getReservation(ctx, p.pool, reservationByID, id)
}

// ListReservations returns tenant's reservations, or everyone's when tenant
// is empty, newest first.
func (p *Postgres) ListReservations(ctx context.Context, tenant string) ([]reservation.Reservation, error) {
	return p.collectReservations(ctx, "list reservations",
		selectReservation+` WHERE ($1 = '' OR r.tenant = $1) ORDER BY r.created_at DESC, r.id DESC`, tenant)
}

// DueToStart returns scheduled reservations whose start is at or before now.
func (p *Postgres) DueToStart(ctx context.Context, now time.Time) ([]reservation.Reservation, error) {
	return p.collectReservations(ctx, "list reservations due to start", dueToStart, now)
}

// Running returns active or degraded reservations whose window contains now.
func (p *Postgres) Running(ctx context.Context, now time.Time) ([]reservation.Reservation, error) {
	return p.collectReservations(ctx, "list running reservations", running, now)
}

// DueToEnd returns active or degraded reservations whose end is at or before
// now.
func (p *Postgres) DueToEnd(ctx context.Context, now time.Time) ([]reservation.Reservation, error) {
	return p.collectReservations(ctx, "list reservations due to end", dueToEnd, now)
}

func (p *Postgres) collectReservations(ctx context.Context, what, sql string, arg any) ([]reservation.Reservation, error) {
	rows, err := p.pool.Query(ctx, sql, arg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	list, err := pgx.CollectRows(rows, scanReservation)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return list, nil
}

// SetReservationStatus records the reconciler's progress. It does not check
// that the transition is sensible, only that s is a status.
func (p *Postgres) SetReservationStatus(ctx context.Context, id string, s reservation.Status) error {
	if !slices.Contains(allStatuses, s) {
		return fmt.Errorf("%w: unknown status %q", reservation.ErrInvalid, s)
	}
	tag, err := p.pool.Exec(ctx, `UPDATE reservations SET status = $2 WHERE id = $1`, id, string(s))
	if err != nil {
		return fmt.Errorf("set status of reservation %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", reservation.ErrNotFound, id)
	}
	return nil
}

// CountReservationsByStatus returns how many reservations are in each status,
// with zero for statuses that have none.
func (p *Postgres) CountReservationsByStatus(ctx context.Context) (map[reservation.Status]int, error) {
	rows, err := p.pool.Query(ctx, `SELECT status, count(*) FROM reservations GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count reservations by status: %w", err)
	}
	counts := make(map[reservation.Status]int, len(allStatuses))
	for _, s := range allStatuses {
		counts[s] = 0
	}
	var status string
	var count int
	_, err = pgx.ForEachRow(rows, []any{&status, &count}, func() error {
		counts[reservation.Status(status)] = count
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan reservation counts: %w", err)
	}
	return counts, nil
}

// CancelReservation cancels a reservation at now. A scheduled one that holds
// no node yet is cancelled outright and its nodes are freed. A running one, or
// a scheduled one already provisioning some of its nodes, keeps its status, so
// the reconciler still releases its nodes, but it ends at now.
func (p *Postgres) CancelReservation(ctx context.Context, id string, now time.Time) (reservation.Reservation, error) {
	var res reservation.Reservation
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		cur, err := getReservation(ctx, tx, lockReservation, id)
		if err != nil {
			return err
		}
		switch cur.Status {
		case reservation.StatusScheduled:
			err = cancelScheduled(ctx, tx, id, now)
		case reservation.StatusActive, reservation.StatusDegraded:
			err = endReservationAt(ctx, tx, id, now)
		default:
			return fmt.Errorf("%w: %s is %s", reservation.ErrNotCancellable, id, cur.Status)
		}
		if err != nil {
			return err
		}
		res, err = getReservation(ctx, tx, reservationByID, id)
		return err
	})
	return res, err
}

// cancelScheduled must run under the reservation lock: StartNode takes it
// too, so no node can start for the reservation between the check and the
// cancel.
func cancelScheduled(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	var started bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM nodes WHERE reservation_id = $1 AND state IN ('provisioning', 'active'))`,
		id).Scan(&started); err != nil {
		return fmt.Errorf("check started nodes of reservation %s: %w", id, err)
	}
	if started {
		return endReservationAt(ctx, tx, id, now)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reservation_nodes WHERE reservation_id = $1`, id); err != nil {
		return fmt.Errorf("free nodes of reservation %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE reservations SET status = $2 WHERE id = $1`,
		id, string(reservation.StatusCancelled)); err != nil {
		return fmt.Errorf("cancel reservation %s: %w", id, err)
	}
	return nil
}

// A period only ever shrinks and never becomes empty: the new end is capped
// at the old end, and is at least one microsecond past the start, so a cancel
// at (or before) the start still leaves a valid period.
const truncatedPeriod = `tstzrange(lower(period),
	least(upper(period), greatest($2::timestamptz, lower(period) + interval '1 microsecond')), '[)')`

func endReservationAt(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET period = `+truncatedPeriod+` WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("truncate reservation %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE reservation_nodes SET period = `+truncatedPeriod+` WHERE reservation_id = $1`, id, now); err != nil {
		return fmt.Errorf("truncate nodes of reservation %s: %w", id, err)
	}
	return nil
}
