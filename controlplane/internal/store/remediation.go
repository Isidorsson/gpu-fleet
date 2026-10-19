package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/remediation"
)

// remediationLockClass is the first key of the advisory lock that serializes
// remediation decisions, so the rate limit is shared by concurrent webhooks
// and by every control plane replica. It is distinct from
// reservationWriteLockClass; both use the two-key form, which cannot collide
// with a lock taken by key alone, such as the reconciler's leader lock.
const remediationLockClass = 0x726d6564

var _ remediation.Store = (*Postgres)(nil)

// ApplyRemediation settles d for one alert and records the outcome, all in one
// transaction. The advisory lock is taken first and the node row second, in
// that order everywhere, so remediations never deadlock with each other or
// with a node transition, which takes only the row lock.
func (p *Postgres) ApplyRemediation(ctx context.Context, d remediation.Decision, limit remediation.Limit, now time.Time) (remediation.Outcome, error) {
	if err := checkRemediation(d, limit); err != nil {
		return "", err
	}
	var outcome remediation.Outcome
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := lockRemediation(ctx, tx); err != nil {
			return err
		}
		var err error
		if outcome, err = quarantineIfAllowed(ctx, tx, d.NodeID, limit, now); err != nil {
			return err
		}
		return insertRemediation(ctx, tx, d, outcome, now)
	})
	if err != nil {
		return "", fmt.Errorf("apply remediation to %s: %w", d.NodeID, err)
	}
	return outcome, nil
}

func checkRemediation(d remediation.Decision, limit remediation.Limit) error {
	if d.Action != remediation.ActionQuarantine {
		return fmt.Errorf("%w: unsupported action %q", remediation.ErrInvalidConfig, d.Action)
	}
	return limit.Validate()
}

func lockRemediation(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, 0)`, remediationLockClass); err != nil {
		return fmt.Errorf("lock remediation: %w", err)
	}
	return nil
}

// quarantineIfAllowed applies hardware_fault to the node, operator-style (the
// node's lease is ignored), unless one of the refusals below holds. They are
// checked in this order: a missing node, an already quarantined one, one that
// cannot take the event, and only then the rate limit, so a refusal that has
// nothing to do with the fleet-wide count never uses up or hides behind it.
func quarantineIfAllowed(ctx context.Context, tx pgx.Tx, nodeID string, limit remediation.Limit, now time.Time) (remediation.Outcome, error) {
	cur, err := getOne(ctx, tx, lockNode, nodeID, scanLocked)
	if errors.Is(err, ErrNotFound) {
		return remediation.OutcomeUnknownNode, nil
	}
	if err != nil {
		return "", err
	}
	if cur.State == node.StateQuarantined {
		return remediation.OutcomeAlreadyQuarantined, nil
	}
	if _, err := node.Next(cur.State, node.EventHardwareFault); err != nil {
		return remediation.OutcomeNotApplicable, nil
	}
	limited, err := quarantineLimitReached(ctx, tx, limit, now)
	if err != nil {
		return "", err
	}
	if limited {
		return remediation.OutcomeRateLimited, nil
	}
	next, err := cur.Apply(node.EventHardwareFault, "")
	if err != nil {
		return "", err
	}
	if err := recordTransition(ctx, tx, cur.Node, next, node.EventHardwareFault); err != nil {
		return "", err
	}
	return remediation.OutcomeQuarantined, nil
}

// quarantineLimitReached reports whether limit.Max quarantines were already
// recorded in the window that ends at now. A quarantine exactly Window old no
// longer counts.
func quarantineLimitReached(ctx context.Context, tx pgx.Tx, limit remediation.Limit, now time.Time) (bool, error) {
	var recent int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM remediation_actions WHERE outcome = $1 AND at > $2`,
		string(remediation.OutcomeQuarantined), now.Add(-limit.Window)).Scan(&recent)
	if err != nil {
		return false, fmt.Errorf("count recent quarantines: %w", err)
	}
	return recent >= limit.Max, nil
}

func insertRemediation(ctx context.Context, tx pgx.Tx, d remediation.Decision, outcome remediation.Outcome, now time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO remediation_actions (node_id, alertname, fingerprint, action, outcome, at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		d.NodeID, d.AlertName, d.Fingerprint, string(d.Action), string(outcome), now)
	if err != nil {
		return fmt.Errorf("insert remediation record: %w", err)
	}
	return nil
}

// ListRemediations returns the newest max records of the remediation audit
// log, newest first.
func (p *Postgres) ListRemediations(ctx context.Context, max int) ([]remediation.Record, error) {
	if max < 1 {
		return nil, fmt.Errorf("list remediations: max must be at least 1, got %d", max)
	}
	rows, err := p.pool.Query(ctx,
		`SELECT node_id, alertname, fingerprint, action, outcome, at
		 FROM remediation_actions ORDER BY at DESC, id DESC LIMIT $1`, max)
	if err != nil {
		return nil, fmt.Errorf("list remediations: %w", err)
	}
	records, err := pgx.CollectRows(rows, scanRemediation)
	if err != nil {
		return nil, fmt.Errorf("scan remediations: %w", err)
	}
	return records, nil
}

func scanRemediation(row pgx.CollectableRow) (remediation.Record, error) {
	var r remediation.Record
	var action, outcome string
	err := row.Scan(&r.NodeID, &r.AlertName, &r.Fingerprint, &action, &outcome, &r.At)
	r.Action, r.Outcome = remediation.Action(action), remediation.Outcome(outcome)
	r.At = r.At.UTC()
	return r, err
}
