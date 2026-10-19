package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

func wantIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestFreeCandidatesByNodeState(t *testing.T) {
	p := openReservationStore(t)
	for _, s := range node.States() {
		registerIn(t, p, "n-"+string(s), s)
	}

	got := freeIDs(t, p, at(time.Hour), at(2*time.Hour))

	wantIDs(t, "free nodes", got, "n-available", "n-inspecting", "n-racked", "n-wiping")
}

func TestFreeCandidatesCarryTheirLocation(t *testing.T) {
	p := openReservationStore(t)
	registerAtIn(t, p, "a", "pod-1", "rack-3", node.StateAvailable)

	free, err := p.FreeCandidates(t.Context(), at(0), at(time.Hour))
	if err != nil {
		t.Fatalf("free candidates: %v", err)
	}
	want := []placement.Candidate{{NodeID: "a", Pod: "pod-1", Rack: "rack-3"}}
	if !slices.Equal(free, want) {
		t.Fatalf("free = %+v, want %+v", free, want)
	}
}

func TestFreeCandidatesRejectsEmptyWindow(t *testing.T) {
	p := openReservationStore(t)
	for _, end := range []time.Time{at(0), at(-time.Hour)} {
		if _, err := p.FreeCandidates(t.Context(), at(0), end); !errors.Is(err, reservation.ErrInvalid) {
			t.Fatalf("end %v: err = %v, want ErrInvalid", end, err)
		}
	}
}

func TestFreeCandidatesExcludeOverlappingReservations(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(time.Hour), at(2*time.Hour), "n1")

	tests := []struct {
		name       string
		start, end time.Duration
		free       bool
	}{
		{"ends exactly when the booking starts", 0, time.Hour, true},
		{"starts exactly when the booking ends", 2 * time.Hour, 3 * time.Hour, true},
		{"overlaps its start", 0, 90 * time.Minute, false},
		{"overlaps its end", 90 * time.Minute, 3 * time.Hour, false},
		{"contains it", 0, 3 * time.Hour, false},
		{"inside it", 75 * time.Minute, 80 * time.Minute, false},
		{"one microsecond into it", 0, time.Hour + time.Microsecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := freeIDs(t, p, at(tt.start), at(tt.end))
			if free := slices.Contains(got, "n1"); free != tt.free {
				t.Fatalf("free = %v (%v), want %v", free, got, tt.free)
			}
		})
	}
}

func TestBusyNodeUnderCurrentReservationFreesWhenItEnds(t *testing.T) {
	for _, state := range []node.State{node.StateProvisioning, node.StateActive} {
		t.Run(string(state), func(t *testing.T) {
			p := openReservationStore(t)
			registerIn(t, p, "n1", state)
			mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n1")

			wantIDs(t, "after the reservation", freeIDs(t, p, at(time.Hour), at(2*time.Hour)), "n1")
			wantIDs(t, "during the reservation", freeIDs(t, p, at(30*time.Minute), at(2*time.Hour)))
			wantIDs(t, "overlapping its last moment", freeIDs(t, p, at(time.Hour-time.Microsecond), at(2*time.Hour)))
		})
	}
}

func TestBusyNodeOutsideAnyCurrentReservationIsNeverFree(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, p *Postgres)
	}{
		{"held by hand", func(*testing.T, *Postgres) {}},
		{"its reservation ended", func(t *testing.T, p *Postgres) {
			mustInsertReservation(t, p, "r1", reservation.StatusEnded, at(-3*time.Hour), at(-2*time.Hour), "n1")
		}},
		{"its reservation starts later", func(t *testing.T, p *Postgres) {
			mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(time.Hour), at(2*time.Hour), "n1")
		}},
		{"its reservation was cancelled at now", func(t *testing.T, p *Postgres) {
			mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Hour), at(0), "n1")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := openReservationStore(t)
			registerIn(t, p, "n1", node.StateActive)
			tt.setup(t, p)

			wantIDs(t, "free nodes", freeIDs(t, p, at(3*time.Hour), at(4*time.Hour)))
		})
	}
}

func TestQuarantinedNodeIsNeverFree(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateQuarantined)
	mustInsertReservation(t, p, "r1", reservation.StatusActive, at(-time.Hour), at(time.Hour), "n1")

	wantIDs(t, "free nodes", freeIDs(t, p, at(2*time.Hour), at(3*time.Hour)))
}

func TestIDsAreOrderedBytewise(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "a1", node.StateAvailable)
	registerIn(t, p, "B1", node.StateAvailable)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(0), at(time.Hour), "a1", "B1")

	got, err := p.GetReservation(t.Context(), "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	wantIDs(t, "reservation nodes", got.NodeIDs, "B1", "a1")
	wantIDs(t, "free nodes", freeIDs(t, p, at(time.Hour), at(2*time.Hour)), "B1", "a1")
}
