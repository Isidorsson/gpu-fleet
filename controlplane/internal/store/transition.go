package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/node"
)

// lockedNode is a node row read under its row lock, with who holds its lease.
// leaseLive is judged by the database clock at read time.
type lockedNode struct {
	node.Node
	leaseOwner string
	leaseLive  bool
}

// fence decides, under the row lock, whether a transition may proceed.
type fence func(lockedNode) error

func noFence(lockedNode) error { return nil }

func heldBy(owner string) fence {
	return func(n lockedNode) error {
		if !n.leaseLive || n.leaseOwner != owner {
			return fmt.Errorf("%w: %s", ErrLeaseLost, n.ID)
		}
		return nil
	}
}

// clock_timestamp(), not now(): now() is the transaction start, which can
// predate a wait for the row lock and so judge an expired lease live.
const lockNode = `SELECT ` + nodeColumns + `,
	coalesce(lease_owner, ''), coalesce(lease_expires_at > clock_timestamp(), false)
	FROM nodes WHERE id = $1 FOR UPDATE`

func scanLocked(row pgx.CollectableRow) (lockedNode, error) {
	var n lockedNode
	var state string
	err := row.Scan(append(nodeDests(&n.Node, &state), &n.leaseOwner, &n.leaseLive)...)
	n.State = node.State(state)
	return n, err
}

// ApplyEvent applies an operator event. It ignores leases.
func (p *Postgres) ApplyEvent(ctx context.Context, id string, ev node.Event, tenant string) (node.Node, error) {
	return p.transition(ctx, id, ev, tenant, noFence)
}

// ApplyEventAsOwner applies a worker's event only while owner still holds the
// node's lease, so a worker that stalled past its lease cannot overwrite what
// its successor did. The lease is kept; the worker releases it when done.
func (p *Postgres) ApplyEventAsOwner(ctx context.Context, id, owner string, ev node.Event, tenant string) (node.Node, error) {
	return p.transition(ctx, id, ev, tenant, heldBy(owner))
}

// transition locks the node row for the whole transition so concurrent
// events on one node are serialized and the audit log matches the state.
func (p *Postgres) transition(ctx context.Context, id string, ev node.Event, tenant string, allow fence) (node.Node, error) {
	var next node.Node
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		cur, err := getOne(ctx, tx, lockNode, id, scanLocked)
		if err != nil {
			return err
		}
		if err := allow(cur); err != nil {
			return err
		}
		if next, err = cur.Apply(ev, tenant); err != nil {
			return err
		}
		return recordTransition(ctx, tx, cur.Node, next, ev)
	})
	return next, err
}

func recordTransition(ctx context.Context, tx pgx.Tx, cur, next node.Node, ev node.Event) error {
	if _, err := tx.Exec(ctx,
		`UPDATE nodes SET state = $2, tenant = nullif($3, ''), reservation_id = nullif($4, ''),
		 updated_at = now(), claim_attempts = 0
		 WHERE id = $1`,
		cur.ID, string(next.State), next.Tenant, next.Reservation); err != nil {
		return fmt.Errorf("update node %s: %w", cur.ID, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO node_events (node_id, from_state, event, to_state, tenant)
		 VALUES ($1, $2, $3, $4, nullif($5, ''))`,
		cur.ID, string(cur.State), string(ev), string(next.State), affectedTenant(cur, next)); err != nil {
		return fmt.Errorf("insert event for %s: %w", cur.ID, err)
	}
	return nil
}

func affectedTenant(before, after node.Node) string {
	if after.Tenant != "" {
		return after.Tenant
	}
	return before.Tenant
}
