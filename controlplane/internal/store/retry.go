package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxPlacementAttempts = 5
	// Attempt n sleeps a random time below n*placementBackoff, so requests
	// that collided once do not collide again in lockstep.
	placementBackoff = 10 * time.Millisecond
)

// retryOnContention runs attempt, which must be one whole transaction, again
// whenever the exclusion constraint rejects it because a concurrent request
// took the same node for an overlapping period. Any other outcome is final.
func retryOnContention(ctx context.Context, attempt func() error) error {
	var err error
	for n := 1; n <= maxPlacementAttempts; n++ {
		if err = attempt(); !isExclusionViolation(err) {
			return err
		}
		if n < maxPlacementAttempts {
			if err := sleepJittered(ctx, n); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("%w: lost %d attempts to concurrent requests: %w",
		ErrPlacementContention, maxPlacementAttempts, err)
}

func isExclusionViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == pgExclusionViolation
}

func sleepJittered(ctx context.Context, attempt int) error {
	timer := time.NewTimer(rand.N(placementBackoff * time.Duration(attempt)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// reservationWriteLockClass is the first key of the advisory lock that
// serializes the writes which book nodes. The two-key form cannot collide
// with a lock taken by key alone, such as the reconciler's leader lock.
const reservationWriteLockClass = 0x72737672

// lockReservationWrites makes the caller wait until no other booking
// transaction is running, and holds that until the caller's transaction ends.
// Without it, two transactions that insert overlapping bookings at the same
// moment each find the other's uncommitted row and wait for it, which
// Postgres only resolves as a deadlock after a second or more. With it, the
// later one sees the earlier one's committed row and fails at once with an
// exclusion violation, which retryOnContention handles. It is taken after the
// candidates are read so that those reads stay concurrent.
func lockReservationWrites(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, 0)`, reservationWriteLockClass); err != nil {
		return fmt.Errorf("lock reservation writes: %w", err)
	}
	return nil
}
