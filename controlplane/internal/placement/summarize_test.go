package placement_test

import (
	"slices"
	"testing"

	"gpu-fleet/controlplane/internal/placement"
)

func TestSummarizeEmpty(t *testing.T) {
	if got := placement.Summarize(nil); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name string
		free []placement.Candidate
		want []placement.PodCapacity
	}{
		{
			name: "single rack",
			free: gen("p1", "r1", 3),
			want: []placement.PodCapacity{{Pod: "p1", Free: 3, LargestRack: 3}},
		},
		{
			name: "largest rack within a pod",
			free: fleet(gen("p1", "r1", 3), gen("p1", "r2", 5), gen("p1", "r3", 1)),
			want: []placement.PodCapacity{{Pod: "p1", Free: 9, LargestRack: 5}},
		},
		{
			name: "sorted by pod regardless of input order",
			free: fleet(gen("p3", "r1", 1), gen("p1", "r1", 2), gen("p2", "r1", 4), gen("p2", "r2", 1)),
			want: []placement.PodCapacity{
				{Pod: "p1", Free: 2, LargestRack: 2},
				{Pod: "p2", Free: 5, LargestRack: 4},
				{Pod: "p3", Free: 1, LargestRack: 1},
			},
		},
		{
			name: "equal rack names in different pods are separate racks",
			free: fleet(gen("p1", "r1", 2), gen("p2", "r1", 2)),
			want: []placement.PodCapacity{
				{Pod: "p1", Free: 2, LargestRack: 2},
				{Pod: "p2", Free: 2, LargestRack: 2},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := placement.Summarize(tc.free)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSummarizeDoesNotMutateInput(t *testing.T) {
	free := fleet(nodes("p2", "r1", "b", "a"), nodes("p1", "r1", "c"))
	before := slices.Clone(free)
	placement.Summarize(free)
	if !slices.Equal(free, before) {
		t.Fatalf("input modified: got %v, want %v", free, before)
	}
}
