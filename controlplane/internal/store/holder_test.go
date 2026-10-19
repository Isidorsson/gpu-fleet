package store

import (
	"errors"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/reservation"
)

func mustStartNode(t *testing.T, p *Postgres, id, nodeID string) node.Node {
	t.Helper()
	n, err := p.StartNode(t.Context(), id, nodeID, testNow)
	if err != nil {
		t.Fatalf("start %s for %s: %v", nodeID, id, err)
	}
	return n
}

func mustGetNode(t *testing.T, p *Postgres, id string) node.Node {
	t.Helper()
	n, err := p.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return n
}

func TestStartNodeProvisionsForTheReservationAndRecordsIt(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(-time.Minute), at(time.Hour), "n01")

	got := mustStartNode(t, p, "r1", "n01")

	if got.State != node.StateProvisioning || got.Tenant != "acme" || got.Reservation != "r1" {
		t.Fatalf("StartNode = %+v, want provisioning for acme held by r1", got)
	}
	if stored := mustGetNode(t, p, "n01"); stored != got {
		t.Fatalf("stored = %+v, want %+v", stored, got)
	}
	events, err := p.Events(t.Context(), "n01")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	last := events[len(events)-1]
	if last.Event != node.EventProvision || last.Tenant != "acme" {
		t.Fatalf("last event = %+v, want provision for acme", last)
	}
}

func TestReservationHolderIsKeptUntilTheWipeSucceeds(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Minute), at(time.Hour), "n01")
	mustStartNode(t, p, "r1", "n01")

	for _, ev := range []node.Event{node.EventProvisionSucceeded, node.EventRelease} {
		if _, err := p.ApplyEvent(t.Context(), "n01", ev, ""); err != nil {
			t.Fatalf("apply %s: %v", ev, err)
		}
		if got := mustGetNode(t, p, "n01"); got.Reservation != "r1" {
			t.Fatalf("after %s = %+v, want still held by r1", ev, got)
		}
	}
	if _, err := p.ApplyEvent(t.Context(), "n01", node.EventWipeSucceeded, ""); err != nil {
		t.Fatalf("apply wipe_succeeded: %v", err)
	}
	if got := mustGetNode(t, p, "n01"); got.Reservation != "" || got.Tenant != "" {
		t.Fatalf("after wipe = %+v, want no reservation or tenant", got)
	}
}

func TestStartNodeRejectsWhatTheReservationDoesNotCover(t *testing.T) {
	tests := []struct {
		name   string
		status reservation.Status
		start  time.Time
		end    time.Time
		nodeID string
	}{
		{"cancelled", reservation.StatusCancelled, at(-time.Minute), at(time.Hour), "n01"},
		{"ended", reservation.StatusEnded, at(-time.Minute), at(time.Hour), "n01"},
		{"node not in the reservation", reservation.StatusScheduled, at(-time.Minute), at(time.Hour), "n02"},
		{"window over", reservation.StatusActive, at(-time.Hour), testNow, "n01"},
		{"window not begun", reservation.StatusScheduled, at(time.Minute), at(time.Hour), "n01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := openReservationStore(t)
			registerFleet(t, p, "n", 2)
			mustInsertReservation(t, p, "r1", tt.status, tt.start, tt.end, "n01")

			_, err := p.StartNode(t.Context(), "r1", tt.nodeID, testNow)

			if !errors.Is(err, reservation.ErrInvalid) {
				t.Fatalf("err = %v, want reservation.ErrInvalid", err)
			}
			if got := mustGetNode(t, p, tt.nodeID); got.State != node.StateAvailable {
				t.Fatalf("node = %+v, want it left available", got)
			}
		})
	}
}

func TestStartNodeOnANodeThatIsNotAvailable(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n01", node.StateWiping)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(-time.Minute), at(time.Hour), "n01")

	_, err := p.StartNode(t.Context(), "r1", "n01", testNow)

	if !errors.Is(err, node.ErrInvalidTransition) {
		t.Fatalf("err = %v, want node.ErrInvalidTransition", err)
	}
}

func TestStartNodeForMissingReservation(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	if _, err := p.StartNode(t.Context(), "res-nope", "n01", testNow); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("err = %v, want reservation.ErrNotFound", err)
	}
}

func TestOperatorProvisionHasNoReservation(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n01", node.StateProvisioning)
	if got := mustGetNode(t, p, "n01"); got.Reservation != "" {
		t.Fatalf("node = %+v, want no reservation", got)
	}
}

func TestRunningListsReservationsInsideTheirWindow(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 6)
	mustInsertReservation(t, p, "active", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n01")
	mustInsertReservation(t, p, "degraded", reservation.StatusDegraded, testNow, at(time.Hour), "n02")
	mustInsertReservation(t, p, "due-to-end", reservation.StatusActive, at(-time.Hour), testNow, "n03")
	mustInsertReservation(t, p, "future", reservation.StatusActive, at(time.Minute), at(time.Hour), "n04")
	mustInsertReservation(t, p, "scheduled", reservation.StatusScheduled, at(-time.Hour), at(time.Hour), "n05")
	mustInsertReservation(t, p, "ended", reservation.StatusEnded, at(-time.Hour), at(time.Hour), "n06")

	got, err := p.Running(t.Context(), testNow)
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	wantIDs(t, "running", ids, "active", "degraded")
	wantIDs(t, "nodes of the first", got[0].NodeIDs, "n01")
}

func TestDropNodeFreesItsBooking(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 2)
	mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n01", "n02")

	for range 2 {
		if err := p.DropNode(t.Context(), "r1", "n01"); err != nil {
			t.Fatalf("drop: %v", err)
		}
	}

	got, err := p.GetReservation(t.Context(), "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	wantIDs(t, "nodes left", got.NodeIDs, "n02")
	if got.Count != 2 {
		t.Errorf("count = %d, want the requested 2 kept", got.Count)
	}
	wantIDs(t, "free for the rest of the window", freeIDs(t, p, testNow, at(time.Hour)), "n01")
}
