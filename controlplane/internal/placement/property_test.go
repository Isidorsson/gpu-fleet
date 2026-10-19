package placement_test

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"gpu-fleet/controlplane/internal/placement"
)

const (
	propertyFleets = 400
	maxPods        = 4
	maxRacksPerPod = 6
	maxNodesInRack = 8
	extraCounts    = 4
)

var allPolicies = []placement.Policy{placement.PolicySameRack, placement.PolicySamePod, placement.PolicyAny}

func randomFleet(rng *rand.Rand) []placement.Candidate {
	freeProb := []float64{0.3, 0.6, 0.9}[rng.IntN(3)]
	var free []placement.Candidate
	for p := range 1 + rng.IntN(maxPods) {
		for r := range 1 + rng.IntN(maxRacksPerPod) {
			for n := range 1 + rng.IntN(maxNodesInRack) {
				if rng.Float64() >= freeProb {
					continue
				}
				free = append(free, placement.Candidate{
					NodeID: fmt.Sprintf("n-%d-%d-%d", p, r, n),
					Pod:    fmt.Sprintf("pod-%d", p),
					Rack:   fmt.Sprintf("rack-%d", r),
				})
			}
		}
	}
	return free
}

func interestingCounts(rng *rand.Rand, s stats) []int {
	counts := []int{1, s.total + 1}
	for _, p := range allPolicies {
		l := s.largest(p)
		counts = append(counts, l, l+1)
	}
	for range extraCounts {
		counts = append(counts, 1+rng.IntN(s.total+2))
	}
	counts = slices.DeleteFunc(counts, func(c int) bool { return c < 1 })
	slices.Sort(counts)
	return slices.Compact(counts)
}

func TestPropertyPlace(t *testing.T) {
	for seed := range uint64(propertyFleets) {
		rng := rand.New(rand.NewPCG(seed, seed*7919+1))
		free := randomFleet(rng)
		s := newStats(free)
		for _, policy := range allPolicies {
			for _, count := range interestingCounts(rng, s) {
				pc := propertyCase{
					t:    t,
					desc: fmt.Sprintf("seed %d policy %s count %d", seed, policy, count),
					req:  placement.Request{Count: count, Policy: policy},
					free: free,
					s:    s,
				}
				pc.check(rng)
			}
		}
	}
}

type propertyCase struct {
	t    *testing.T
	desc string
	req  placement.Request
	free []placement.Candidate
	s    stats
}

func (pc propertyCase) check(rng *rand.Rand) {
	pc.t.Helper()
	got, err := placement.Place(pc.req, pc.free)
	if pc.s.largest(pc.req.Policy) < pc.req.Count {
		pc.checkFailure(got, err)
	} else {
		pc.checkSuccess(got, err)
	}
	pc.checkDeterminism(rng, got, err)
}

func (pc propertyCase) checkFailure(got []string, err error) {
	pc.t.Helper()
	var capErr *placement.CapacityError
	switch {
	case got != nil:
		pc.t.Fatalf("%s: got %v, want capacity error", pc.desc, got)
	case !errors.Is(err, placement.ErrInsufficientCapacity), !errors.As(err, &capErr):
		pc.t.Fatalf("%s: err = %v, want *CapacityError", pc.desc, err)
	case capErr.Largest != pc.s.largest(pc.req.Policy):
		pc.t.Fatalf("%s: Largest = %d, want %d", pc.desc, capErr.Largest, pc.s.largest(pc.req.Policy))
	case capErr.Largest >= pc.req.Count:
		pc.t.Fatalf("%s: Largest %d is not below Count", pc.desc, capErr.Largest)
	}
}

func (pc propertyCase) checkSuccess(got []string, err error) {
	pc.t.Helper()
	if err != nil {
		pc.t.Fatalf("%s: unexpected error %v", pc.desc, err)
	}
	byID := make(map[string]placement.Candidate, len(pc.free))
	for _, c := range pc.free {
		byID[c.NodeID] = c
	}
	if len(got) != pc.req.Count || !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		pc.t.Fatalf("%s: want %d sorted distinct IDs, got %v", pc.desc, pc.req.Count, got)
	}
	chosen := make([]placement.Candidate, 0, len(got))
	for _, id := range got {
		c, ok := byID[id]
		if !ok {
			pc.t.Fatalf("%s: %q was not free", pc.desc, id)
		}
		chosen = append(chosen, c)
	}
	pc.checkShape(newStats(chosen))
}

func (pc propertyCase) checkShape(used stats) {
	pc.t.Helper()
	switch pc.req.Policy {
	case placement.PolicySameRack:
		pc.checkSameRack(used)
	case placement.PolicySamePod:
		pc.checkSamePod(used)
	default:
		pc.checkAny(used)
	}
}

func (pc propertyCase) checkSameRack(used stats) {
	pc.t.Helper()
	if len(used.racks) != 1 {
		pc.t.Fatalf("%s: spans %d racks", pc.desc, len(used.racks))
	}
	key := slices.Collect(maps.Keys(used.racks))[0]
	want := smallestFit(slices.Collect(maps.Values(pc.s.racks)), pc.req.Count)
	if pc.s.racks[key] != want {
		pc.t.Fatalf("%s: rack %v has %d free, best fit has %d", pc.desc, key, pc.s.racks[key], want)
	}
}

func (pc propertyCase) checkSamePod(used stats) {
	pc.t.Helper()
	if len(used.pods) != 1 {
		pc.t.Fatalf("%s: spans %d pods", pc.desc, len(used.pods))
	}
	pod := slices.Collect(maps.Keys(used.pods))[0]
	want := smallestFit(slices.Collect(maps.Values(pc.s.pods)), pc.req.Count)
	if pc.s.pods[pod] != want {
		pc.t.Fatalf("%s: pod %s has %d free, best fit has %d", pc.desc, pod, pc.s.pods[pod], want)
	}
	pc.checkInsidePod(pod, used)
}

func (pc propertyCase) checkInsidePod(pod string, used stats) {
	pc.t.Helper()
	podRacks := make(map[rackKey]int)
	for k, n := range pc.s.racks {
		if k.pod == pod {
			podRacks[k] = n
		}
	}
	if fit := smallestFit(slices.Collect(maps.Values(podRacks)), pc.req.Count); fit != -1 {
		pc.checkSingleBestFit(used, fit)
		return
	}
	checkOrderedConsumption(pc, podRacks, used.racks, false)
}

func (pc propertyCase) checkSingleBestFit(used stats, fit int) {
	pc.t.Helper()
	if len(used.racks) != 1 {
		pc.t.Fatalf("%s: a rack fits but %d racks were used", pc.desc, len(used.racks))
	}
	for k := range used.racks {
		if pc.s.racks[k] != fit {
			pc.t.Fatalf("%s: rack %v has %d free, best fit has %d", pc.desc, k, pc.s.racks[k], fit)
		}
	}
}

func (pc propertyCase) checkAny(used stats) {
	pc.t.Helper()
	if want := pc.s.minPods(pc.req.Count); len(used.pods) != want {
		pc.t.Fatalf("%s: used %d pods, minimum is %d", pc.desc, len(used.pods), want)
	}
	if len(used.pods) == 1 {
		pc.checkSamePod(used)
		return
	}
	checkOrderedConsumption(pc, pc.s.pods, used.pods, true)
}

// checkOrderedConsumption verifies the multi-unit rules: at most one rack (or
// pod) is partly used, and the units used are the smallest ones when
// descending is false (racks inside a pod) or the largest when it is true
// (pods in an any span).
func checkOrderedConsumption[K comparable](pc propertyCase, free, used map[K]int, descending bool) {
	pc.t.Helper()
	partial := 0
	usedSizes, unusedSizes := []int{}, []int{}
	for k, n := range free {
		switch u := used[k]; {
		case u == 0:
			unusedSizes = append(unusedSizes, n)
		case u < n:
			partial++
			usedSizes = append(usedSizes, n)
		default:
			usedSizes = append(usedSizes, n)
		}
	}
	if partial > 1 {
		pc.t.Fatalf("%s: %d units partially consumed", pc.desc, partial)
	}
	if len(usedSizes) == 0 || len(unusedSizes) == 0 {
		return
	}
	lo, hi := slices.Min(usedSizes), slices.Max(usedSizes)
	if descending && lo < slices.Max(unusedSizes) {
		pc.t.Fatalf("%s: used a unit of %d free while one of %d was left", pc.desc, lo, slices.Max(unusedSizes))
	}
	if !descending && hi > slices.Min(unusedSizes) {
		pc.t.Fatalf("%s: used a unit of %d free while one of %d was left", pc.desc, hi, slices.Min(unusedSizes))
	}
}

func (pc propertyCase) checkDeterminism(rng *rand.Rand, got []string, err error) {
	pc.t.Helper()
	shuffled := slices.Clone(pc.free)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	got2, err2 := placement.Place(pc.req, shuffled)
	if !slices.Equal(got, got2) || fmt.Sprint(err) != fmt.Sprint(err2) {
		pc.t.Fatalf("%s: shuffled input changed the outcome: %v/%v vs %v/%v", pc.desc, got, err, got2, err2)
	}
}

func TestPropertySummarize(t *testing.T) {
	for seed := range uint64(propertyFleets) {
		rng := rand.New(rand.NewPCG(seed, seed*104729+3))
		free := randomFleet(rng)
		s := newStats(free)
		got := placement.Summarize(free)
		if len(got) != len(s.pods) || !slices.IsSortedFunc(got, comparePod) {
			t.Fatalf("seed %d: got %+v for %d pods", seed, got, len(s.pods))
		}
		for _, pc := range got {
			if pc.Free != s.pods[pc.Pod] || pc.LargestRack != largestRackIn(s, pc.Pod) {
				t.Fatalf("seed %d: %+v disagrees with oracle", seed, pc)
			}
		}
	}
}

func comparePod(a, b placement.PodCapacity) int {
	switch {
	case a.Pod < b.Pod:
		return -1
	case a.Pod > b.Pod:
		return 1
	}
	return 0
}

func largestRackIn(s stats, pod string) int {
	largest := 0
	for k, n := range s.racks {
		if k.pod == pod {
			largest = max(largest, n)
		}
	}
	return largest
}
