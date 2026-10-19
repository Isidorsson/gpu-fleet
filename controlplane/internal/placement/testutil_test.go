package placement_test

import (
	"fmt"
	"slices"

	"gpu-fleet/controlplane/internal/placement"
)

var _ placement.Placer = placement.Place

// gen returns n candidates named "<pod>-<rack>-01" .. "<pod>-<rack>-<n>".
func gen(pod, rack string, n int) []placement.Candidate {
	out := make([]placement.Candidate, 0, n)
	for _, id := range idsOf(pod, rack, 1, n) {
		out = append(out, placement.Candidate{NodeID: id, Pod: pod, Rack: rack})
	}
	return out
}

// idsOf returns the IDs gen produces for node numbers from..to inclusive.
func idsOf(pod, rack string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s-%s-%02d", pod, rack, i))
	}
	return out
}

func nodes(pod, rack string, ids ...string) []placement.Candidate {
	out := make([]placement.Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, placement.Candidate{NodeID: id, Pod: pod, Rack: rack})
	}
	return out
}

func fleet(groups ...[]placement.Candidate) []placement.Candidate {
	return slices.Concat(groups...)
}

func sorted(groups ...[]string) []string {
	out := slices.Concat(groups...)
	slices.Sort(out)
	return out
}

func without(free []placement.Candidate, ids []string) []placement.Candidate {
	var out []placement.Candidate
	for _, c := range free {
		if !slices.Contains(ids, c.NodeID) {
			out = append(out, c)
		}
	}
	return out
}
