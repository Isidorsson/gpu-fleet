package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

const (
	reservationIDPrefix = "res-"
	reservationIDBytes  = 6
)

// CreateReservation books req.Count nodes chosen by place from the nodes free
// for the whole window, in one transaction. Another request taking a chosen
// node between the read and the insert makes the exclusion constraint reject
// the insert; the whole transaction then runs again against the new state, up
// to maxPlacementAttempts times (ErrPlacementContention after that). An error
// from place, such as a *placement.CapacityError, is returned as is.
func (p *Postgres) CreateReservation(ctx context.Context, req reservation.Request, place placement.Placer) (reservation.Reservation, error) {
	if _, err := req.Validate(req.StartsAt); err != nil {
		return reservation.Reservation{}, err
	}
	var res reservation.Reservation
	err := retryOnContention(ctx, func() error {
		var err error
		res, err = p.createOnce(ctx, req, place)
		return err
	})
	return res, err
}

func (p *Postgres) createOnce(ctx context.Context, req reservation.Request, place placement.Placer) (reservation.Reservation, error) {
	var res reservation.Reservation
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		free, err := freeCandidates(ctx, tx, req.StartsAt, req.EndsAt, p.now())
		if err != nil {
			return err
		}
		ids, err := place(placement.Request{Count: req.Count, Policy: req.Policy}, free)
		if err != nil {
			return err
		}
		if err := checkPlacement(ids, req.Count, free); err != nil {
			return err
		}
		id, err := newReservationID()
		if err != nil {
			return err
		}
		if err := lockReservationWrites(ctx, tx); err != nil {
			return err
		}
		if err := insertReservation(ctx, tx, id, req); err != nil {
			return err
		}
		if err := insertReservationNodes(ctx, tx, id, slices.Sorted(slices.Values(ids)), req.StartsAt, req.EndsAt); err != nil {
			return err
		}
		res, err = getReservation(ctx, tx, reservationByID, id)
		return err
	})
	return res, err
}

// checkPlacement rejects a placer result that books a node which was not
// offered, books one twice, or books the wrong number.
func checkPlacement(ids []string, count int, free []placement.Candidate) error {
	if len(ids) != count {
		return fmt.Errorf("%w: got %d nodes, want %d", ErrBadPlacement, len(ids), count)
	}
	offered := make(map[string]bool, len(free))
	for _, c := range free {
		offered[c.NodeID] = true
	}
	for _, id := range ids {
		if !offered[id] {
			return fmt.Errorf("%w: node %s was not offered", ErrBadPlacement, id)
		}
		delete(offered, id)
	}
	return nil
}

func newReservationID() (string, error) {
	b := make([]byte, reservationIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate reservation id: %w", err)
	}
	return reservationIDPrefix + hex.EncodeToString(b), nil
}

func insertReservation(ctx context.Context, tx pgx.Tx, id string, req reservation.Request) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO reservations (id, tenant, node_count, policy, period, status)
		 VALUES ($1, $2, $3, $4, tstzrange($5, $6, '[)'), $7)`,
		id, req.Tenant, req.Count, string(req.Policy), req.StartsAt, req.EndsAt,
		string(reservation.StatusScheduled))
	if err != nil {
		return fmt.Errorf("insert reservation: %w", err)
	}
	return nil
}

func insertReservationNodes(ctx context.Context, tx pgx.Tx, id string, nodeIDs []string, start, end time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO reservation_nodes (reservation_id, node_id, period)
		 SELECT $1::text, node_id, tstzrange($3, $4, '[)') FROM unnest($2::text[]) AS node_id`,
		id, nodeIDs, start, end)
	if err != nil {
		return fmt.Errorf("book nodes for reservation %s: %w", id, err)
	}
	return nil
}
