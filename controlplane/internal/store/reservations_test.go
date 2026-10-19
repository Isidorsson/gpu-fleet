package store

import (
	"errors"
	"maps"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

func reservationIDs(rs []reservation.Reservation) []string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

func TestGetMissingReservation(t *testing.T) {
	p := openReservationStore(t)
	if _, err := p.GetReservation(t.Context(), "res-nope"); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("err = %v, want reservation.ErrNotFound", err)
	}
}

func TestListReservationsNewestFirstAndByTenant(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 3)
	var ids []string
	for _, tenant := range []string{"acme", "globex", "acme"} {
		req := request(1, placement.PolicyAny, at(time.Hour), at(2*time.Hour))
		req.Tenant = tenant
		res, err := p.CreateReservation(t.Context(), req, takeFirst)
		if err != nil {
			t.Fatalf("create for %s: %v", tenant, err)
		}
		ids = append(ids, res.ID)
	}

	all, err := p.ListReservations(t.Context(), "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	wantIDs(t, "all", reservationIDs(all), ids[2], ids[1], ids[0])

	acme, err := p.ListReservations(t.Context(), "acme")
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	wantIDs(t, "acme", reservationIDs(acme), ids[2], ids[0])

	none, err := p.ListReservations(t.Context(), "initech")
	if err != nil || len(none) != 0 {
		t.Fatalf("list initech = %v, %v; want empty", none, err)
	}
}

func TestDueToStartIncludesTheBoundary(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 6)
	mustInsertReservation(t, p, "early", reservation.StatusScheduled, at(-time.Minute), at(time.Hour), "n01")
	mustInsertReservation(t, p, "exact", reservation.StatusScheduled, at(0), at(time.Hour), "n02")
	mustInsertReservation(t, p, "just-after", reservation.StatusScheduled, at(time.Microsecond), at(time.Hour), "n03")
	mustInsertReservation(t, p, "running", reservation.StatusActive, at(-time.Minute), at(time.Hour), "n04")
	mustInsertReservation(t, p, "dropped", reservation.StatusCancelled, at(-time.Minute), at(time.Hour), "n05")

	due, err := p.DueToStart(t.Context(), testNow)
	if err != nil {
		t.Fatalf("due to start: %v", err)
	}

	wantIDs(t, "due to start", reservationIDs(due), "early", "exact")
	wantIDs(t, "nodes", due[0].NodeIDs, "n01")
}

func TestDueToEndIncludesTheBoundary(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 6)
	mustInsertReservation(t, p, "exact", reservation.StatusActive, at(-time.Hour), at(0), "n01")
	mustInsertReservation(t, p, "late", reservation.StatusDegraded, at(-time.Hour), at(-time.Minute), "n02")
	mustInsertReservation(t, p, "just-after", reservation.StatusActive, at(-time.Hour), at(time.Microsecond), "n03")
	mustInsertReservation(t, p, "never-started", reservation.StatusScheduled, at(-2*time.Hour), at(-time.Hour), "n04")
	mustInsertReservation(t, p, "done", reservation.StatusEnded, at(-2*time.Hour), at(-time.Hour), "n05")

	due, err := p.DueToEnd(t.Context(), testNow)
	if err != nil {
		t.Fatalf("due to end: %v", err)
	}

	wantIDs(t, "due to end", reservationIDs(due), "late", "exact")
}

func TestSetReservationStatus(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(0), at(time.Hour), "n01")

	for _, s := range allStatuses {
		if err := p.SetReservationStatus(t.Context(), "r1", s); err != nil {
			t.Fatalf("set %s: %v", s, err)
		}
		got, err := p.GetReservation(t.Context(), "r1")
		if err != nil || got.Status != s {
			t.Fatalf("after set %s: status = %q, %v", s, got.Status, err)
		}
	}
}

func TestSetReservationStatusRejectsUnknownStatusAndReservation(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 1)
	mustInsertReservation(t, p, "r1", reservation.StatusActive, at(0), at(time.Hour), "n01")

	if err := p.SetReservationStatus(t.Context(), "r1", "paused"); !errors.Is(err, reservation.ErrInvalid) {
		t.Fatalf("unknown status: err = %v, want ErrInvalid", err)
	}
	if err := p.SetReservationStatus(t.Context(), "res-nope", reservation.StatusEnded); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("unknown reservation: err = %v, want ErrNotFound", err)
	}
	if got, _ := p.GetReservation(t.Context(), "r1"); got.Status != reservation.StatusActive {
		t.Fatalf("status = %q after rejected updates, want active", got.Status)
	}
}

func TestCountReservationsByStatus(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 4)

	empty, err := p.CountReservationsByStatus(t.Context())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	zeros := map[reservation.Status]int{}
	for _, s := range allStatuses {
		zeros[s] = 0
	}
	if !maps.Equal(empty, zeros) {
		t.Fatalf("empty store counts = %v, want every status at 0", empty)
	}

	mustInsertReservation(t, p, "a", reservation.StatusScheduled, at(0), at(time.Hour), "n01")
	mustInsertReservation(t, p, "b", reservation.StatusScheduled, at(0), at(time.Hour), "n02")
	mustInsertReservation(t, p, "c", reservation.StatusDegraded, at(0), at(time.Hour), "n03")
	mustInsertReservation(t, p, "d", reservation.StatusEnded, at(0), at(time.Hour), "n04")

	got, err := p.CountReservationsByStatus(t.Context())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	want := maps.Clone(zeros)
	want[reservation.StatusScheduled], want[reservation.StatusDegraded], want[reservation.StatusEnded] = 2, 1, 1
	if !maps.Equal(got, want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
}
