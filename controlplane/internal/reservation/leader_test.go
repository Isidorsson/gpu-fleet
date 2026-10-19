package reservation

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const leaderHandoverTimeout = 5 * time.Second

// Integration tests run against TEST_DATABASE_URL and are skipped without it.
// Each test locks a random key so tests and a running control plane cannot
// collide.
func openLeaderPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomLockKey(t *testing.T) int64 {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
}

func newTestLeader(t *testing.T, pool *pgxpool.Pool, key int64) *PostgresLeader {
	t.Helper()
	l := NewPostgresLeader(pool, key)
	t.Cleanup(func() {
		if err := l.Close(context.Background()); err != nil {
			t.Errorf("close leader: %v", err)
		}
	})
	return l
}

func mustAcquire(t *testing.T, l *PostgresLeader, want bool, what string) {
	t.Helper()
	got, err := l.TryAcquire(context.Background())
	if err != nil {
		t.Fatalf("%s: TryAcquire: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s: TryAcquire = %v, want %v", what, got, want)
	}
}

func TestPostgresLeaderIsExclusivePerKey(t *testing.T) {
	pool := openLeaderPool(t)
	key := randomLockKey(t)
	a := newTestLeader(t, pool, key)
	b := newTestLeader(t, pool, key)

	mustAcquire(t, a, true, "first contender")
	mustAcquire(t, b, false, "second contender")
	mustAcquire(t, a, true, "leader asked again")
	mustAcquire(t, b, false, "second contender again")
}

func TestPostgresLeaderKeysAreIndependent(t *testing.T) {
	pool := openLeaderPool(t)
	a := newTestLeader(t, pool, randomLockKey(t))
	b := newTestLeader(t, pool, randomLockKey(t))

	mustAcquire(t, a, true, "key a")
	mustAcquire(t, b, true, "key b")
}

func TestPostgresLeaderCloseHandsOverLeadership(t *testing.T) {
	pool := openLeaderPool(t)
	key := randomLockKey(t)
	a := newTestLeader(t, pool, key)
	b := newTestLeader(t, pool, key)

	mustAcquire(t, a, true, "leader")
	mustAcquire(t, b, false, "follower")
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	acquireWithin(t, b, leaderHandoverTimeout)
	mustAcquire(t, a, false, "closed leader competing again")
}

func TestPostgresLeaderLosesLeadershipWhenItsConnectionDies(t *testing.T) {
	pool := openLeaderPool(t)
	key := randomLockKey(t)
	a := newTestLeader(t, pool, key)
	b := newTestLeader(t, pool, key)

	mustAcquire(t, a, true, "leader")
	pid := a.conn.Conn().PgConn().PID()
	if _, err := pool.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate leader backend: %v", err)
	}

	acquireWithin(t, b, leaderHandoverTimeout)
	mustAcquire(t, a, false, "old leader after its connection died")
}

func acquireWithin(t *testing.T, l *PostgresLeader, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := l.TryAcquire(context.Background())
		if err != nil {
			t.Fatalf("TryAcquire: %v", err)
		}
		if got {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not handed over within %s", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
