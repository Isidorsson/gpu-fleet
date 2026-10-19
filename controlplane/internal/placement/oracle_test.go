package placement_test

import (
	"maps"
	"slices"

	"gpu-fleet/controlplane/internal/placement"
)

type rackKey struct{ pod, rack string }

// stats counts free nodes per rack and pod without using the package under
// test, so property tests can check outcomes against an independent view.
type stats struct {
	racks map[rackKey]int
	pods  map[string]int
	total int
}

func newStats(free []placement.Candidate) stats {
	s := stats{racks: map[rackKey]int{}, pods: map[string]int{}, total: len(free)}
	for _, c := range free {
		s.racks[rackKey{c.Pod, c.Rack}]++
		s.pods[c.Pod]++
	}
	return s
}

func (s stats) largest(policy placement.Policy) int {
	switch policy {
	case placement.PolicySameRack:
		return slices.Max(append([]int{0}, slices.Collect(maps.Values(s.racks))...))
	case placement.PolicySamePod:
		return slices.Max(append([]int{0}, slices.Collect(maps.Values(s.pods))...))
	}
	return s.total
}

// minPods is the fewest pods whose free counts sum to at least count.
func (s stats) minPods(count int) int {
	counts := slices.Collect(maps.Values(s.pods))
	slices.SortFunc(counts, func(a, b int) int { return b - a })
	sum := 0
	for i, c := range counts {
		sum += c
		if sum >= count {
			return i + 1
		}
	}
	return -1
}

// smallestFit is the smallest value in counts that is at least count, or -1.
func smallestFit(counts []int, count int) int {
	best := -1
	for _, c := range counts {
		if c >= count && (best == -1 || c < best) {
			best = c
		}
	}
	return best
}
