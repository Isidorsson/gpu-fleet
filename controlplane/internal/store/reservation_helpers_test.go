package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

// testNow is the fixed clock of reservation tests, so windows are plain
// offsets from it and nothing depends on the wall clock.
var testNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

func at(offset time.Duration) time.Time { return testNow.Add(offset) }

func openReservationStore(t *testing.T) *Postgres {
	t.Helper()
	return openReservationStoreWithConns(t, 0)
}

func openReservationStoreWithConns(t *testing.T, maxConns int32) *Postgres {
	t.Helper()
	p := openTestStoreWithConns(t, maxConns)
	p.now = func() time.Time { return testNow }
	return p
}

func registerFleet(t *testing.T, p *Postgres, prefix string, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%02d", prefix, i+1)
		registerIn(t, p, ids[i], node.StateAvailable)
	}
	return ids
}

func fixedPlacer(ids ...string) placement.Placer {
	return func(placement.Request, []placement.Candidate) ([]string, error) { return ids, nil }
}

// insertReservationRows writes a reservation and its node rows directly, to set
// up states CreateReservation would not produce.
func insertReservationRows(ctx context.Context, p *Postgres, id string, status reservation.Status, start, end time.Time, nodeIDs ...string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO reservations (id, tenant, node_count, policy, period, status)
		 VALUES ($1, 'acme', $2, 'any', tstzrange($3, $4, '[)'), $5)`,
		id, len(nodeIDs), start, end, string(status))
	if err != nil {
		return err
	}
	for _, nodeID := range nodeIDs {
		_, err := p.pool.Exec(ctx,
			`INSERT INTO reservation_nodes (reservation_id, node_id, period)
			 VALUES ($1, $2, tstzrange($3, $4, '[)'))`, id, nodeID, start, end)
		if err != nil {
			return err
		}
	}
	return nil
}

func mustInsertReservation(t *testing.T, p *Postgres, id string, status reservation.Status, start, end time.Time, nodeIDs ...string) {
	t.Helper()
	if err := insertReservationRows(context.Background(), p, id, status, start, end, nodeIDs...); err != nil {
		t.Fatalf("insert reservation %s: %v", id, err)
	}
}

func freeIDs(t *testing.T, p *Postgres, start, end time.Time) []string {
	t.Helper()
	free, err := p.FreeCandidates(context.Background(), start, end)
	if err != nil {
		t.Fatalf("free candidates: %v", err)
	}
	return candidateIDs(free)
}

func candidateIDs(cs []placement.Candidate) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.NodeID)
	}
	return ids
}

func countRows(t *testing.T, p *Postgres, table string) int {
	t.Helper()
	var n int
	if err := p.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// takeFirst is the simplest placer that honors the contract: the first Count
// free nodes, sorted, or a CapacityError.
func takeFirst(req placement.Request, free []placement.Candidate) ([]string, error) {
	if len(free) < req.Count {
		return nil, &placement.CapacityError{Policy: req.Policy, Requested: req.Count, Largest: len(free)}
	}
	ids := candidateIDs(free[:req.Count])
	slices.Sort(ids)
	return ids, nil
}

// spyPlacer wraps a placer and records what it was asked. before, if set,
// runs on every call with the 1-based call number, after the candidates were
// read and before the placement is returned.
type spyPlacer struct {
	inner  placement.Placer
	before func(call int, ids []string)

	mu    sync.Mutex
	calls int
	seen  [][]placement.Candidate
}

func (s *spyPlacer) place(req placement.Request, free []placement.Candidate) ([]string, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.seen = append(s.seen, slices.Clone(free))
	s.mu.Unlock()

	ids, err := s.inner(req, free)
	if err == nil && s.before != nil {
		s.before(call, ids)
	}
	return ids, err
}

func (s *spyPlacer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *spyPlacer) lastSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return candidateIDs(s.seen[len(s.seen)-1])
}

func request(count int, policy placement.Policy, start, end time.Time) reservation.Request {
	return reservation.Request{Tenant: "acme", Count: count, Policy: policy, StartsAt: start, EndsAt: end}
}
