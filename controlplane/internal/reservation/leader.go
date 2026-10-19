package reservation

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresLeader elects a leader with a session-level advisory lock held on
// one dedicated pooled connection. The lock disappears with the connection,
// so a crashed or partitioned leader frees it without any cleanup. It takes
// one connection from the pool for as long as it leads.
type PostgresLeader struct {
	pool *pgxpool.Pool
	key  int64

	mu   sync.Mutex
	conn *pgxpool.Conn
}

func NewPostgresLeader(pool *pgxpool.Pool, key int64) *PostgresLeader {
	return &PostgresLeader{pool: pool, key: key}
}

// TryAcquire reports whether this process holds the lock. A leader re-checks
// its connection every call and, if it died, falls back to competing again.
func (l *PostgresLeader) TryAcquire(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		if l.conn.Ping(ctx) == nil {
			return true, nil
		}
		_ = l.dropConn(ctx) // the ping already failed; the close error adds nothing
	}
	return l.compete(ctx)
}

func (l *PostgresLeader) compete(ctx context.Context) (bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		conn.Release()
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return false, nil
	}
	l.conn = conn
	return true, nil
}

// Close gives up leadership by closing the locking connection.
func (l *PostgresLeader) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropConn(ctx)
}

// dropConn closes the connection instead of returning it to the pool, which
// would keep the session lock alive for the next borrower.
func (l *PostgresLeader) dropConn(ctx context.Context) error {
	if l.conn == nil {
		return nil
	}
	conn := l.conn.Hijack()
	l.conn = nil
	if err := conn.Close(ctx); err != nil {
		return fmt.Errorf("close leader connection: %w", err)
	}
	return nil
}
