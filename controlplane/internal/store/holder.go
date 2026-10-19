package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/reservation"
)

// shareReservation lets node starts for one reservation run side by side
// while blocking a concurrent cancel or replacement, which lock it for update.
const shareReservation = reservationByID + ` FOR SHARE OF r`

// StartNode provisions nodeID for reservation id's tenant and records the
// reservation as the node's holder. The node must be booked by the
// reservation at now and the reservation must not be over.
func (p *Postgres) StartNode(ctx context.Context, id, nodeID string, now time.Time) (node.Node, error) {
	var next node.Node
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		res, err := getReservation(ctx, tx, shareReservation, id)
		if err != nil {
			return err
		}
		if err := checkStartable(ctx, tx, res, nodeID, now); err != nil {
			return err
		}
		cur, err := getOne(ctx, tx, lockNode, nodeID, scanLocked)
		if err != nil {
			return err
		}
		if next, err = cur.ProvisionFor(res.Tenant, res.ID); err != nil {
			return err
		}
		return recordTransition(ctx, tx, cur.Node, next, node.EventProvision)
	})
	return next, err
}

func checkStartable(ctx context.Context, q pgx.Tx, res reservation.Reservation, nodeID string, now time.Time) error {
	switch res.Status {
	case reservation.StatusEnded, reservation.StatusCancelled:
		return fmt.Errorf("%w: %s is %s", reservation.ErrInvalid, res.ID, res.Status)
	}
	var booked bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM reservation_nodes
		 WHERE reservation_id = $1 AND node_id = $2 AND period @> $3::timestamptz)`,
		res.ID, nodeID, now).Scan(&booked); err != nil {
		return fmt.Errorf("check booking of %s in %s: %w", nodeID, res.ID, err)
	}
	if !booked {
		return fmt.Errorf("%w: %s does not book %s at %s", reservation.ErrInvalid, res.ID, nodeID, now.Format(time.RFC3339))
	}
	return nil
}

// DropNode removes nodeID from reservation id, so a node that left and could
// not be replaced stops blocking capacity. The reservation keeps its count.
func (p *Postgres) DropNode(ctx context.Context, id, nodeID string) error {
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM reservation_nodes WHERE reservation_id = $1 AND node_id = $2`, id, nodeID); err != nil {
		return fmt.Errorf("drop node %s from %s: %w", nodeID, id, err)
	}
	return nil
}
