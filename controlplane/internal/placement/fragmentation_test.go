package placement_test

import (
	"slices"
	"sync"
	"testing"

	"gpu-fleet/controlplane/internal/placement"
)

func mustPlace(t *testing.T, count int, policy placement.Policy, free []placement.Candidate) []string {
	t.Helper()
	got, err := placement.Place(placement.Request{Count: count, Policy: policy}, free)
	if err != nil {
		t.Fatalf("Place(%d, %s): %v", count, policy, err)
	}
	return got
}

// A same_pod request that no single rack can hold must eat the small racks
// first so the whole 4-node rack survives for a later same_rack request.
func TestSamePodKeepsWholeRackForSameRack(t *testing.T) {
	free := fleet(
		gen("p1", "r1", 4), gen("p1", "r2", 4),
		gen("p1", "r3", 2), gen("p1", "r4", 2),
	)

	placed := mustPlace(t, 6, placement.PolicySamePod, free)
	wantPlaced := sorted(idsOf("p1", "r1", 1, 2), idsOf("p1", "r3", 1, 2), idsOf("p1", "r4", 1, 2))
	if !slices.Equal(placed, wantPlaced) {
		t.Fatalf("same_pod placed %v, want %v", placed, wantPlaced)
	}

	next := mustPlace(t, 4, placement.PolicySameRack, without(free, placed))
	if want := idsOf("p1", "r2", 1, 4); !slices.Equal(next, want) {
		t.Fatalf("same_rack after same_pod got %v, want %v", next, want)
	}
}

// A small same_pod request goes to the smallest pod that fits, leaving the
// 8-node pod whole for a later same_pod request.
func TestSamePodKeepsWholePodForSamePod(t *testing.T) {
	free := fleet(gen("p1", "r1", 3), gen("p2", "r1", 8))

	placed := mustPlace(t, 3, placement.PolicySamePod, free)
	if want := idsOf("p1", "r1", 1, 3); !slices.Equal(placed, want) {
		t.Fatalf("first placement got %v, want %v", placed, want)
	}

	next := mustPlace(t, 8, placement.PolicySamePod, without(free, placed))
	if want := idsOf("p2", "r1", 1, 8); !slices.Equal(next, want) {
		t.Fatalf("second placement got %v, want %v", next, want)
	}
}

func TestSameRackBestFitKeepsLargeRackWhole(t *testing.T) {
	free := fleet(gen("p1", "r1", 2), gen("p1", "r2", 8))

	placed := mustPlace(t, 2, placement.PolicySameRack, free)
	next := mustPlace(t, 8, placement.PolicySameRack, without(free, placed))
	if want := idsOf("p1", "r2", 1, 8); !slices.Equal(next, want) {
		t.Fatalf("got %v, want %v", next, want)
	}
}

func TestPlaceConcurrentCallsAgree(t *testing.T) {
	free := fleet(gen("p1", "r1", 3), gen("p1", "r2", 2), gen("p2", "r1", 4))
	req := placement.Request{Count: 6, Policy: placement.PolicyAny}
	want := mustPlace(t, req.Count, req.Policy, free)

	const workers = 8
	results := make([][]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			results[i], _ = placement.Place(req, free)
		})
	}
	wg.Wait()
	for i, got := range results {
		if !slices.Equal(got, want) {
			t.Fatalf("worker %d got %v, want %v", i, got, want)
		}
	}
}
