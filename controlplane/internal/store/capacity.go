package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

// $1, $2: the window [start, end). $3: the current time. A node whose tenant
// holds it is only a candidate when a reservation covering now accounts for
// that, because then the reservation's end bounds how long it stays busy.
// The C collation matches the byte order placement sorts IDs in.
const freeNodes = `
SELECT n.id, n.pod, n.rack
FROM nodes n
WHERE (n.state IN ('racked', 'inspecting', 'available', 'wiping')
       OR (n.state IN ('provisioning', 'active')
           AND EXISTS (SELECT 1 FROM reservation_nodes rn
                       WHERE rn.node_id = n.id AND rn.period @> $3::timestamptz)))
  AND NOT EXISTS (SELECT 1 FROM reservation_nodes rn
                  WHERE rn.node_id = n.id AND rn.period && tstzrange($1, $2, '[)'))
ORDER BY n.id COLLATE "C"`

// FreeCandidates returns the nodes that can be booked for all of [start, end).
// Which busy nodes count as current is judged by the store's clock.
func (p *Postgres) FreeCandidates(ctx context.Context, start, end time.Time) ([]placement.Candidate, error) {
	return freeCandidates(ctx, p.pool, start, end, p.now())
}

func freeCandidates(ctx context.Context, q querier, start, end, now time.Time) ([]placement.Candidate, error) {
	if !end.After(start) {
		return nil, fmt.Errorf("%w: window must end after it starts", reservation.ErrInvalid)
	}
	rows, err := q.Query(ctx, freeNodes, start, end, now)
	if err != nil {
		return nil, fmt.Errorf("list free nodes: %w", err)
	}
	free, err := pgx.CollectRows(rows, pgx.RowToStructByPos[placement.Candidate])
	if err != nil {
		return nil, fmt.Errorf("scan free nodes: %w", err)
	}
	return free, nil
}

// ReplaceNode swaps oldNodeID in reservation id for a node chosen by place
// from those free for the rest of the reservation, [max(now, start), end),
// and returns its ID. With policy same_pod or same_rack the candidates are
// limited to the pod or rack the reservation's other nodes are in. Like
// CreateReservation it retries when it loses a race for the chosen node.
func (p *Postgres) ReplaceNode(ctx context.Context, id, oldNodeID string, now time.Time, place placement.Placer) (string, error) {
	var newID string
	err := retryOnContention(ctx, func() error {
		var err error
		newID, err = p.replaceOnce(ctx, id, oldNodeID, now, place)
		return err
	})
	return newID, err
}

func (p *Postgres) replaceOnce(ctx context.Context, id, oldNodeID string, now time.Time, place placement.Placer) (string, error) {
	var newID string
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		res, err := getReservation(ctx, tx, lockReservation, id)
		if err != nil {
			return err
		}
		if err := checkReplaceable(res, oldNodeID); err != nil {
			return err
		}
		start := later(now, res.StartsAt)
		free, err := replacementCandidates(ctx, tx, res, oldNodeID, start, now)
		if err != nil {
			return err
		}
		ids, err := place(placement.Request{Count: 1, Policy: res.Policy}, free)
		if err != nil {
			return err
		}
		if err := checkPlacement(ids, 1, free); err != nil {
			return err
		}
		newID = ids[0]
		if err := lockReservationWrites(ctx, tx); err != nil {
			return err
		}
		return swapNode(ctx, tx, id, oldNodeID, newID, start, res.EndsAt)
	})
	if err != nil {
		return "", err
	}
	return newID, nil
}

func checkReplaceable(res reservation.Reservation, oldNodeID string) error {
	switch res.Status {
	case reservation.StatusEnded, reservation.StatusCancelled:
		return fmt.Errorf("%w: %s is %s", reservation.ErrInvalid, res.ID, res.Status)
	}
	if !slices.Contains(res.NodeIDs, oldNodeID) {
		return fmt.Errorf("%w: node %s is not in %s", reservation.ErrInvalid, oldNodeID, res.ID)
	}
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// replacementCandidates is empty once the window has run out, which makes the
// placer report that nothing can be placed.
func replacementCandidates(ctx context.Context, q querier, res reservation.Reservation, oldNodeID string, start, now time.Time) ([]placement.Candidate, error) {
	if !start.Before(res.EndsAt) {
		return nil, nil
	}
	free, err := freeCandidates(ctx, q, start, res.EndsAt, now)
	if err != nil {
		return nil, err
	}
	free = slices.DeleteFunc(free, func(c placement.Candidate) bool { return c.NodeID == oldNodeID })
	kept, err := reservedCandidates(ctx, q, res.ID, oldNodeID)
	if err != nil {
		return nil, err
	}
	return nearNeighbors(res.Policy, kept, free), nil
}

func reservedCandidates(ctx context.Context, q querier, id, exceptNodeID string) ([]placement.Candidate, error) {
	rows, err := q.Query(ctx,
		`SELECT n.id, n.pod, n.rack FROM reservation_nodes rn JOIN nodes n ON n.id = rn.node_id
		 WHERE rn.reservation_id = $1 AND rn.node_id <> $2`, id, exceptNodeID)
	if err != nil {
		return nil, fmt.Errorf("list nodes of reservation %s: %w", id, err)
	}
	kept, err := pgx.CollectRows(rows, pgx.RowToStructByPos[placement.Candidate])
	if err != nil {
		return nil, fmt.Errorf("scan nodes of reservation %s: %w", id, err)
	}
	return kept, nil
}

// nearNeighbors keeps the candidates that sit where the reservation's other
// nodes already are: in their pod for same_pod, their rack for same_rack.
func nearNeighbors(policy placement.Policy, neighbors, free []placement.Candidate) []placement.Candidate {
	if len(neighbors) == 0 {
		return free
	}
	zones := make(map[[2]string]bool, len(neighbors))
	for _, n := range neighbors {
		zones[zoneOf(policy, n)] = true
	}
	return slices.DeleteFunc(free, func(c placement.Candidate) bool { return !zones[zoneOf(policy, c)] })
}

// zoneOf names the part of the fabric policy confines a reservation to. Racks
// are only unique within a pod. Under any, all nodes share one zone.
func zoneOf(policy placement.Policy, c placement.Candidate) [2]string {
	switch policy {
	case placement.PolicySameRack:
		return [2]string{c.Pod, c.Rack}
	case placement.PolicySamePod:
		return [2]string{c.Pod}
	}
	return [2]string{}
}

func swapNode(ctx context.Context, tx pgx.Tx, id, oldNodeID, newNodeID string, start, end time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE reservation_nodes SET node_id = $3, period = tstzrange($4, $5, '[)')
		 WHERE reservation_id = $1 AND node_id = $2`,
		id, oldNodeID, newNodeID, start, end)
	if err != nil {
		return fmt.Errorf("replace node %s with %s in %s: %w", oldNodeID, newNodeID, id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: node %s is not in %s", reservation.ErrInvalid, oldNodeID, id)
	}
	return nil
}
