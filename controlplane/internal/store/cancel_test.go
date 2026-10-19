package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

// nodeRowPeriod reads one node row's period.
func nodeRowPeriod(t *testing.T, p *Postgres, reservationID, nodeID string) (start, end time.Time) {
	t.Helper()
	err := p.pool.QueryRow(context.Background(),
		`SELECT lower(period), upper(period) FROM reservation_nodes WHERE reservation_id = $1 AND node_id = $2`,
		reservationID, nodeID).Scan(&start, &end)
	if err != nil {
		t.Fatalf("read period of %s in %s: %v", nodeID, reservationID, err)
	}
	return start, end
}

func TestCancelScheduledReservationFreesItsNodes(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 4)
	window := request(2, placement.PolicyAny, at(time.Hour), at(2*time.Hour))
	made, err := p.CreateReservation(t.Context(), window, takeFirst)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wantIDs(t, "free while reserved", freeIDs(t, p, at(time.Hour), at(2*time.Hour)), "n03", "n04")

	got, err := p.CancelReservation(t.Context(), made.ID, testNow)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if got.Status != reservation.StatusCancelled || len(got.NodeIDs) != 0 {
		t.Fatalf("cancelled = %+v, want status cancelled and no nodes", got)
	}
	if n := countRows(t, p, "reservation_nodes"); n != 0 {
		t.Fatalf("%d node rows left, want 0", n)
	}
	wantIDs(t, "free after cancel", freeIDs(t, p, at(time.Hour), at(2*time.Hour)), "n01", "n02", "n03", "n04")
	stored, err := p.GetReservation(t.Context(), made.ID)
	if err != nil || stored.Status != reservation.StatusCancelled {
		t.Fatalf("stored = %+v, %v; want it kept as cancelled", stored, err)
	}
}

func TestCancelRunningReservationEndsItNow(t *testing.T) {
	for _, status := range []reservation.Status{reservation.StatusActive, reservation.StatusDegraded} {
		t.Run(string(status), func(t *testing.T) {
			p := openReservationStore(t)
			registerFleet(t, p, "n", 2)
			mustInsertReservation(t, p, "r1", status, at(-time.Hour), at(time.Hour), "n01", "n02")

			got, err := p.CancelReservation(t.Context(), "r1", testNow)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}

			if got.Status != status {
				t.Errorf("status = %q, want it left as %q for the reconciler", got.Status, status)
			}
			if !got.StartsAt.Equal(at(-time.Hour)) || !got.EndsAt.Equal(testNow) {
				t.Errorf("period = %v..%v, want %v..%v", got.StartsAt, got.EndsAt, at(-time.Hour), testNow)
			}
			wantIDs(t, "nodes", got.NodeIDs, "n01", "n02")
			for _, id := range got.NodeIDs {
				if s, e := nodeRowPeriod(t, p, "r1", id); !s.Equal(at(-time.Hour)) || !e.Equal(testNow) {
					t.Errorf("node %s held %v..%v, want %v..%v", id, s, e, at(-time.Hour), testNow)
				}
			}
			due, err := p.DueToEnd(t.Context(), testNow)
			if err != nil || len(due) != 1 {
				t.Errorf("due to end = %v, %v; want the cancelled reservation", due, err)
			}
			wantIDs(t, "free from now", freeIDs(t, p, testNow, at(time.Hour)), "n01", "n02")
		})
	}
}

func TestCancelRunningReservationKeepsAValidPeriod(t *testing.T) {
	tests := []struct {
		name    string
		now     time.Time
		wantEnd time.Time
	}{
		{"after the end it never extends", at(3 * time.Hour), at(time.Hour)},
		{"at the end", at(time.Hour), at(time.Hour)},
		{"at the start it is not left empty", at(-time.Hour), at(-time.Hour + time.Microsecond)},
		{"before the start it is not left empty", at(-2 * time.Hour), at(-time.Hour + time.Microsecond)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := openReservationStore(t)
			registerFleet(t, p, "n", 1)
			mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n01")

			got, err := p.CancelReservation(t.Context(), "r1", tt.now)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if !got.StartsAt.Equal(at(-time.Hour)) || !got.EndsAt.Equal(tt.wantEnd) {
				t.Fatalf("period = %v..%v, want %v..%v", got.StartsAt, got.EndsAt, at(-time.Hour), tt.wantEnd)
			}
			if _, end := nodeRowPeriod(t, p, "r1", "n01"); !end.Equal(tt.wantEnd) {
				t.Fatalf("node row ends %v, want %v", end, tt.wantEnd)
			}
		})
	}
}

func TestCancelEndedOrCancelledReservationIsRejected(t *testing.T) {
	for _, status := range []reservation.Status{reservation.StatusEnded, reservation.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			p := openReservationStore(t)
			registerFleet(t, p, "n", 1)
			mustInsertReservation(t, p, "r1", status, at(-time.Hour), at(time.Hour), "n01")

			_, err := p.CancelReservation(t.Context(), "r1", testNow)

			if !errors.Is(err, reservation.ErrNotCancellable) {
				t.Fatalf("err = %v, want ErrNotCancellable", err)
			}
			got, err := p.GetReservation(t.Context(), "r1")
			if err != nil || got.Status != status || !got.EndsAt.Equal(at(time.Hour)) {
				t.Fatalf("reservation = %+v, %v; want it untouched", got, err)
			}
		})
	}
}

func TestCancelMissingReservation(t *testing.T) {
	p := openReservationStore(t)
	if _, err := p.CancelReservation(t.Context(), "res-nope", testNow); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("err = %v, want reservation.ErrNotFound", err)
	}
}

func TestCancelScheduledReservationMidStartEndsItNow(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 2)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(-time.Minute), at(time.Hour), "n01", "n02")
	mustStartNode(t, p, "r1", "n01")

	got, err := p.CancelReservation(t.Context(), "r1", testNow)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if got.Status != reservation.StatusScheduled || !got.EndsAt.Equal(testNow) {
		t.Fatalf("cancelled = %+v, want scheduled ending now for the reconciler to wind down", got)
	}
	wantIDs(t, "nodes", got.NodeIDs, "n01", "n02")
	for _, id := range got.NodeIDs {
		if _, end := nodeRowPeriod(t, p, "r1", id); !end.Equal(testNow) {
			t.Errorf("node %s held until %v, want %v", id, end, testNow)
		}
	}
	due, err := p.DueToStart(t.Context(), testNow)
	if err != nil || len(due) != 1 || due[0].ID != "r1" {
		t.Fatalf("due to start = %v, %v; want r1", due, err)
	}
}

func TestCancelScheduledIgnoresNodesHeldByAnotherReservation(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	mustInsertReservation(t, p, "r0", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n01")
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(time.Hour), at(2*time.Hour), "n01")
	mustStartNode(t, p, "r0", "n01")

	got, err := p.CancelReservation(t.Context(), "r1", testNow)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if got.Status != reservation.StatusCancelled || len(got.NodeIDs) != 0 {
		t.Fatalf("cancelled = %+v, want cancelled outright", got)
	}
}
