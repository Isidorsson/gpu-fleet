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

// registerTopology builds two pods. Rack names repeat across pods on purpose:
// a rack is identified by its pod too.
//
//	p1/r1: a1 a2 a3    p1/r2: b1 b2    p2/r1: c1 c2
func registerTopology(t *testing.T, p *Postgres) {
	t.Helper()
	for _, n := range []struct{ id, pod, rack string }{
		{"a1", "p1", "r1"}, {"a2", "p1", "r1"}, {"a3", "p1", "r1"},
		{"b1", "p1", "r2"}, {"b2", "p1", "r2"},
		{"c1", "p2", "r1"}, {"c2", "p2", "r1"},
	} {
		registerAtIn(t, p, n.id, n.pod, n.rack, node.StateAvailable)
	}
}

func reserve(t *testing.T, p *Postgres, policy placement.Policy, ids ...string) reservation.Reservation {
	t.Helper()
	res, err := p.CreateReservation(t.Context(),
		request(len(ids), policy, at(time.Hour), at(2*time.Hour)), fixedPlacer(ids...))
	if err != nil {
		t.Fatalf("reserve %v: %v", ids, err)
	}
	return res
}

func TestReplaceNodeStaysInTheRemainingNodesZone(t *testing.T) {
	tests := []struct {
		name       string
		policy     placement.Policy
		nodes      []string
		replace    string
		wantOffers []string
		wantNew    string
	}{
		{"same_rack stays in the rack", placement.PolicySameRack, []string{"a1", "a2"}, "a1", []string{"a3"}, "a3"},
		{"same_pod stays in the pod", placement.PolicySamePod, []string{"a1", "b1"}, "a1", []string{"a2", "a3", "b2"}, "a2"},
		{"any is unrestricted", placement.PolicyAny, []string{"a1", "c1"}, "c1", []string{"a2", "a3", "b1", "b2", "c2"}, "a2"},
		{"a lone node has no zone", placement.PolicySameRack, []string{"b1"}, "b1", []string{"a1", "a2", "a3", "b2", "c1", "c2"}, "a1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := openReservationStore(t)
			registerTopology(t, p)
			res := reserve(t, p, tt.policy, tt.nodes...)
			spy := &spyPlacer{inner: takeFirst}

			got, err := p.ReplaceNode(t.Context(), res.ID, tt.replace, testNow, spy.place)
			if err != nil {
				t.Fatalf("replace: %v", err)
			}

			if got != tt.wantNew {
				t.Errorf("replacement = %q, want %q", got, tt.wantNew)
			}
			wantIDs(t, "candidates offered", spy.lastSeen(), tt.wantOffers...)
			after, err := p.GetReservation(t.Context(), res.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !after.StartsAt.Equal(res.StartsAt) || !after.EndsAt.Equal(res.EndsAt) || after.Count != res.Count {
				t.Errorf("reservation changed: %+v -> %+v", res, after)
			}
			kept := slices.DeleteFunc(slices.Clone(tt.nodes), func(id string) bool { return id == tt.replace })
			wantIDs(t, "nodes", after.NodeIDs, slices.Sorted(slices.Values(append(kept, tt.wantNew)))...)
		})
	}
}

func TestReplaceNodeReportsCapacityErrorWhenTheZoneIsFull(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicySameRack, "b1", "b2")
	spy := &spyPlacer{inner: takeFirst}

	got, err := p.ReplaceNode(t.Context(), res.ID, "b1", testNow, spy.place)

	capErr, ok := errors.AsType[*placement.CapacityError](err)
	if !ok || capErr.Requested != 1 || capErr.Policy != placement.PolicySameRack {
		t.Fatalf("err = %v, want a *CapacityError for 1 node under same_rack", err)
	}
	if got != "" {
		t.Errorf("replacement = %q, want none", got)
	}
	after, _ := p.GetReservation(t.Context(), res.ID)
	wantIDs(t, "nodes after the failed replace", after.NodeIDs, "b1", "b2")
}

func TestReplaceNodeOnlyNeedsTheRestOfTheWindow(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res, err := p.CreateReservation(t.Context(),
		request(2, placement.PolicySameRack, at(-2*time.Hour), at(2*time.Hour)), fixedPlacer("a1", "a2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.SetReservationStatus(t.Context(), res.ID, reservation.StatusActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// a3 was busy only before now, which no longer matters.
	mustInsertReservation(t, p, "past", reservation.StatusEnded, at(-2*time.Hour), at(-time.Hour), "a3")

	got, err := p.ReplaceNode(t.Context(), res.ID, "a1", testNow, takeFirst)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if got != "a3" {
		t.Fatalf("replacement = %q, want a3, whose booking ended before now", got)
	}
	start, end := nodeRowPeriod(t, p, res.ID, "a3")
	if !start.Equal(testNow) || !end.Equal(at(2*time.Hour)) {
		t.Fatalf("a3 held %v..%v, want %v..%v", start, end, testNow, at(2*time.Hour))
	}
	if start, _ := nodeRowPeriod(t, p, res.ID, "a2"); !start.Equal(at(-2 * time.Hour)) {
		t.Fatalf("a2 hold now starts %v, want it untouched", start)
	}
}

func TestReplaceNodeSkipsNodesBookedLaterInTheWindow(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicySameRack, "a1", "a2")
	mustInsertReservation(t, p, "later", reservation.StatusScheduled, at(90*time.Minute), at(3*time.Hour), "a3")

	_, err := p.ReplaceNode(t.Context(), res.ID, "a1", testNow, takeFirst)

	if !errors.Is(err, placement.ErrInsufficientCapacity) {
		t.Fatalf("err = %v, want insufficient capacity: a3 is booked inside the window", err)
	}
}

func TestReplaceNodeAfterTheWindowEndedHasNoCapacity(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicyAny, "a1", "a2")

	_, err := p.ReplaceNode(t.Context(), res.ID, "a1", at(3*time.Hour), takeFirst)

	if !errors.Is(err, placement.ErrInsufficientCapacity) {
		t.Fatalf("err = %v, want insufficient capacity once the window is over", err)
	}
}

func TestReplaceNodeRejectsWhatItCannotReplace(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicyAny, "a1", "a2")
	mustInsertReservation(t, p, "done", reservation.StatusEnded, at(-3*time.Hour), at(-2*time.Hour), "b1")

	tests := []struct {
		name    string
		id, old string
		want    error
	}{
		{"unknown reservation", "res-nope", "a1", reservation.ErrNotFound},
		{"node not in it", res.ID, "b2", reservation.ErrInvalid},
		{"ended reservation", "done", "b1", reservation.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &spyPlacer{inner: takeFirst}
			if _, err := p.ReplaceNode(t.Context(), tt.id, tt.old, testNow, spy.place); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if n := spy.callCount(); n != 0 {
				t.Fatalf("placer called %d times, want 0", n)
			}
		})
	}
}

func TestReplaceNodeRejectsBadPlacements(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicySameRack, "a1", "a2")

	for name, ids := range map[string][]string{
		"a node out of the zone": {"c1"},
		"two nodes":              {"a3", "a3"},
	} {
		if _, err := p.ReplaceNode(t.Context(), res.ID, "a1", testNow, fixedPlacer(ids...)); !errors.Is(err, ErrBadPlacement) {
			t.Errorf("%s: err = %v, want ErrBadPlacement", name, err)
		}
	}
}

func TestReplaceNodeRetriesAfterLosingARace(t *testing.T) {
	p := openReservationStore(t)
	registerTopology(t, p)
	res := reserve(t, p, placement.PolicySamePod, "a1", "b1")
	spy := &spyPlacer{inner: takeFirst, before: rivalTakes(t, p, 1, at(time.Hour), at(2*time.Hour))}

	got, err := p.ReplaceNode(t.Context(), res.ID, "a1", testNow, spy.place)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	if got != "a3" {
		t.Fatalf("replacement = %q, want a3 after a2 was taken by a rival", got)
	}
	if n := spy.callCount(); n != 2 {
		t.Fatalf("placer called %d times, want 2", n)
	}
	wantIDs(t, "candidates on the retry", spy.lastSeen(), "a3", "b2")
}
