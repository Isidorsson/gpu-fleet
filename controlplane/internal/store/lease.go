package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/node"
)

// Lease times come from the database clock, never the caller's, so
// replicas with skewed clocks agree on who holds a node.

// States in which a node needs a worker. The literal list must match the
// predicate of nodes_claimable_idx for the planner to use that index.
const claimableStates = `('racked', 'inspecting', 'provisioning', 'wiping')`

var errBadLease = errors.New("invalid lease request")

// ClaimWork leases one node that needs a worker (state racked, inspecting,
// provisioning or wiping) whose lease is free or expired. Nodes claimed the
// fewest times since their last transition go first, then oldest updated_at,
// so a node that keeps killing its workers cannot starve the others.
func (p *Postgres) ClaimWork(ctx context.Context, owner string, ttl time.Duration) (node.Node, error) {
	if owner == "" || ttl <= 0 {
		return node.Node{}, fmt.Errorf("%w: need an owner and a positive ttl", errBadLease)
	}
	rows, err := p.pool.Query(ctx,
		`UPDATE nodes SET lease_owner = $1, lease_expires_at = now() + make_interval(secs => $2),
		     claim_attempts = claim_attempts + 1
		 WHERE id = (
		     SELECT id FROM nodes
		     WHERE state IN `+claimableStates+`
		       AND (lease_expires_at IS NULL OR lease_expires_at <= now())
		     ORDER BY claim_attempts, updated_at, id
		     LIMIT 1
		     FOR UPDATE SKIP LOCKED)
		 RETURNING `+nodeColumns,
		owner, ttl.Seconds())
	if err != nil {
		return node.Node{}, fmt.Errorf("claim work: %w", err)
	}
	n, err := pgx.CollectExactlyOneRow(rows, scanNode)
	if errors.Is(err, pgx.ErrNoRows) {
		return node.Node{}, ErrNoWork
	}
	if err != nil {
		return node.Node{}, fmt.Errorf("claim work: %w", err)
	}
	return n, nil
}

// RenewLease extends owner's lease to ttl from now. It returns ErrLeaseLost
// if owner does not hold the lease or it has already expired.
func (p *Postgres) RenewLease(ctx context.Context, id, owner string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: ttl must be positive", errBadLease)
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE nodes SET lease_expires_at = now() + make_interval(secs => $3)
		 WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > now()`,
		id, owner, ttl.Seconds())
	if err != nil {
		return fmt.Errorf("renew lease on %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, id)
	}
	return nil
}

// ReleaseLease frees the node for the next ClaimWork. It clears the lease only
// if owner holds it and is a no-op otherwise, so it is safe to defer.
func (p *Postgres) ReleaseLease(ctx context.Context, id, owner string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE nodes SET lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = $1 AND lease_owner = $2`,
		id, owner)
	if err != nil {
		return fmt.Errorf("release lease on %s: %w", id, err)
	}
	return nil
}
