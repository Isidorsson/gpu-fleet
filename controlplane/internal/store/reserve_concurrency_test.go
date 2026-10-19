package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

const barrierTimeout = 10 * time.Second

// TestConcurrentReservationsNeverDoubleBook has more requests than the fleet
// can hold, all wanting the same nodes. The placer is deliberately naive
// (first nodes in ID order), so every request collides with every other and
// only the exclusion constraint keeps the bookings apart.
func TestConcurrentReservationsNeverDoubleBook(t *testing.T) {
	const (
		fleet  = 20
		perRes = 4
		racers = 8
		fits   = fleet / perRes
	)
	p := openReservationStoreWithConns(t, racers+2)
	registerFleet(t, p, "n", fleet)
	req := request(perRes, placement.PolicyAny, at(time.Hour), at(2*time.Hour))

	// Each racer's first placement waits for all the others, so every one of
	// them has read the same free nodes and picked the same four. That makes
	// the first lost race certain rather than a matter of timing.
	var arrived atomic.Int32
	allArrived := make(chan struct{})
	spy := &spyPlacer{inner: takeFirst, before: func(call int, _ []string) {
		if call > racers {
			return
		}
		if arrived.Add(1) == racers {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-time.After(barrierTimeout):
			t.Errorf("racer %d: others never reached the placer", call)
		}
	}}

	results := make([]reservation.Reservation, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() { results[i], errs[i] = p.CreateReservation(t.Context(), req, spy.place) })
	}
	wg.Wait()

	booked := map[string]string{}
	winners, capacityErrs := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			for _, id := range results[i].NodeIDs {
				if other, dup := booked[id]; dup {
					t.Errorf("node %s booked by both %s and %s", id, other, results[i].ID)
				}
				booked[id] = results[i].ID
			}
			if len(results[i].NodeIDs) != perRes {
				t.Errorf("%s has %d nodes, want %d", results[i].ID, len(results[i].NodeIDs), perRes)
			}
		case errors.Is(err, placement.ErrInsufficientCapacity):
			capacityErrs++
		case errors.Is(err, ErrPlacementContention):
			// Out of attempts before seeing the fleet was full; allowed.
		default:
			t.Errorf("racer %d: unexpected error %v", i, err)
		}
	}

	if winners != fits || len(booked) != fleet {
		t.Errorf("%d reservations booked %d nodes, want %d booking all %d", winners, len(booked), fits, fleet)
	}
	if capacityErrs == 0 {
		t.Errorf("no racer was told the fleet was full")
	}
	assertNoDoubleBooking(t, p, fleet)
	if calls := spy.callCount(); calls < 2*racers-1 {
		t.Errorf("placer called %d times, want at least %d: the first-round losers must have retried", calls, 2*racers-1)
	}
	t.Logf("%d placer calls, %d winners, %d capacity errors, %d gave up",
		spy.callCount(), winners, capacityErrs, racers-winners-capacityErrs)
}

func assertNoDoubleBooking(t *testing.T, p *Postgres, wantRows int) {
	t.Helper()
	rows, err := p.pool.Query(context.Background(),
		`SELECT a.node_id, a.reservation_id, b.reservation_id
		 FROM reservation_nodes a JOIN reservation_nodes b
		   ON a.node_id = b.node_id AND a.reservation_id < b.reservation_id AND a.period && b.period`)
	if err != nil {
		t.Fatalf("query overlaps: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var node, a, b string
		if err := rows.Scan(&node, &a, &b); err != nil {
			t.Fatalf("scan overlap: %v", err)
		}
		t.Errorf("node %s is in overlapping reservations %s and %s", node, a, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("overlap rows: %v", err)
	}
	if n := countRows(t, p, "reservation_nodes"); n != wantRows {
		t.Errorf("%d node rows, want %d", n, wantRows)
	}
}
