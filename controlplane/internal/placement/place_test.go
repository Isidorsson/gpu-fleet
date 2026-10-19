package placement_test

import (
	"slices"
	"testing"

	"gpu-fleet/controlplane/internal/placement"
)

type placeCase struct {
	name  string
	count int
	free  []placement.Candidate
	want  []string
}

func runPlaceCases(t *testing.T, policy placement.Policy, cases []placeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placement.Place(placement.Request{Count: tc.count, Policy: policy}, tc.free)
			if err != nil {
				t.Fatalf("Place: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlaceSameRack(t *testing.T) {
	runPlaceCases(t, placement.PolicySameRack, []placeCase{
		{
			name:  "smallest sufficient rack wins",
			count: 2,
			free:  fleet(gen("p1", "r1", 4), gen("p2", "r1", 3), gen("p1", "r2", 5)),
			want:  idsOf("p2", "r1", 1, 2),
		},
		{
			name:  "exact fit beats larger rack",
			count: 2,
			free:  fleet(gen("p1", "r1", 4), gen("p1", "r2", 2)),
			want:  idsOf("p1", "r2", 1, 2),
		},
		{
			name:  "ties go to lowest rack within a pod",
			count: 3,
			free:  fleet(gen("p1", "r3", 3), gen("p1", "r2", 3), gen("p1", "r1", 4)),
			want:  idsOf("p1", "r2", 1, 3),
		},
		{
			name:  "ties go to lowest pod before lowest rack",
			count: 3,
			free:  fleet(gen("p2", "r1", 3), gen("p1", "r9", 3)),
			want:  idsOf("p1", "r9", 1, 3),
		},
		{
			name:  "same rack name in two pods is two racks",
			count: 2,
			free:  fleet(gen("p2", "r1", 2), gen("p1", "r1", 2), gen("p2", "r2", 5)),
			want:  idsOf("p1", "r1", 1, 2),
		},
		{
			name:  "nodes are taken in ascending ID order",
			count: 2,
			free:  nodes("p1", "r1", "c", "a", "d", "b"),
			want:  []string{"a", "b"},
		},
		{
			name:  "single node comes from the smallest rack",
			count: 1,
			free:  fleet(gen("p1", "r1", 5), gen("p1", "r2", 1), gen("p2", "r1", 3)),
			want:  idsOf("p1", "r2", 1, 1),
		},
		{
			name:  "count equal to rack size takes the whole rack",
			count: 3,
			free:  fleet(gen("p1", "r1", 3), gen("p1", "r2", 2)),
			want:  idsOf("p1", "r1", 1, 3),
		},
	})
}

func TestPlaceSamePod(t *testing.T) {
	runPlaceCases(t, placement.PolicySamePod, []placeCase{
		{
			name:  "smallest sufficient pod wins",
			count: 3,
			free:  fleet(gen("p1", "r1", 5), gen("p2", "r1", 3), gen("p3", "r1", 4)),
			want:  idsOf("p2", "r1", 1, 3),
		},
		{
			name:  "pod size is the sum of its racks",
			count: 4,
			free:  fleet(gen("p1", "r1", 2), gen("p1", "r2", 2), gen("p2", "r1", 5)),
			want:  sorted(idsOf("p1", "r1", 1, 2), idsOf("p1", "r2", 1, 2)),
		},
		{
			name:  "pod ties go to lowest pod",
			count: 3,
			free:  fleet(gen("p2", "r1", 3), gen("p1", "r1", 3)),
			want:  idsOf("p1", "r1", 1, 3),
		},
		{
			name:  "single fitting rack uses best-fit rack",
			count: 3,
			free:  fleet(gen("p1", "r1", 5), gen("p1", "r2", 3), gen("p1", "r3", 2)),
			want:  idsOf("p1", "r2", 1, 3),
		},
		{
			name:  "best-fit rack ties go to lowest rack",
			count: 3,
			free:  fleet(gen("p1", "r2", 3), gen("p1", "r1", 3), gen("p1", "r3", 4)),
			want:  idsOf("p1", "r1", 1, 3),
		},
		{
			name:  "no single rack: ascending free count, each consumed fully",
			count: 4,
			free:  fleet(gen("p1", "r1", 3), gen("p1", "r2", 1), gen("p1", "r3", 2)),
			want:  sorted(idsOf("p1", "r2", 1, 1), idsOf("p1", "r3", 1, 2), idsOf("p1", "r1", 1, 1)),
		},
		{
			name:  "ascending rack ties go to lowest rack and stop when satisfied",
			count: 4,
			free:  fleet(gen("p1", "r3", 3), gen("p1", "r2", 2), gen("p1", "r1", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 2), idsOf("p1", "r2", 1, 2)),
		},
		{
			name:  "last rack is partially consumed from its lowest IDs",
			count: 4,
			free:  fleet(gen("p1", "r2", 3), gen("p1", "r1", 3)),
			want:  sorted(idsOf("p1", "r1", 1, 3), idsOf("p1", "r2", 1, 1)),
		},
		{
			name:  "count equal to pod size takes every rack",
			count: 4,
			free:  fleet(gen("p1", "r1", 2), gen("p1", "r2", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 2), idsOf("p1", "r2", 1, 2)),
		},
	})
}

func TestPlaceAny(t *testing.T) {
	runPlaceCases(t, placement.PolicyAny, []placeCase{
		{
			name:  "a fitting pod behaves like same_pod",
			count: 3,
			free:  fleet(gen("p1", "r1", 5), gen("p2", "r1", 3), gen("p3", "r1", 4)),
			want:  idsOf("p2", "r1", 1, 3),
		},
		{
			name:  "spans pods in descending free count",
			count: 8,
			free:  fleet(gen("p1", "r1", 2), gen("p2", "r1", 5), gen("p3", "r1", 4)),
			want:  sorted(idsOf("p2", "r1", 1, 5), idsOf("p3", "r1", 1, 3)),
		},
		{
			name:  "remainder comes from the best-fit pod, leaving larger pods whole",
			count: 8,
			free:  fleet(gen("p1", "r1", 6), gen("p2", "r1", 3), gen("p3", "r1", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 6), idsOf("p3", "r1", 1, 2)),
		},
		{
			name:  "remainder takes the smallest pod that still fits it",
			count: 8,
			free:  fleet(gen("p1", "r1", 5), gen("p2", "r1", 4), gen("p3", "r1", 3), gen("p4", "r1", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 5), idsOf("p3", "r1", 1, 3)),
		},
		{
			name:  "best-fit ties go to lowest pod",
			count: 7,
			free:  fleet(gen("p1", "r1", 5), gen("p3", "r1", 2), gen("p2", "r1", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 5), idsOf("p2", "r1", 1, 2)),
		},
		{
			name:  "pod ties go to lowest pod",
			count: 5,
			free:  fleet(gen("p2", "r1", 3), gen("p1", "r1", 3)),
			want:  sorted(idsOf("p1", "r1", 1, 3), idsOf("p2", "r1", 1, 2)),
		},
		{
			name:  "uses as few pods as possible",
			count: 9,
			free:  fleet(gen("p1", "r1", 4), gen("p2", "r1", 4), gen("p3", "r1", 4), gen("p4", "r1", 1)),
			want:  sorted(idsOf("p1", "r1", 1, 4), idsOf("p2", "r1", 1, 4), idsOf("p4", "r1", 1, 1)),
		},
		{
			name:  "partial pod with a fitting rack uses best-fit rack",
			count: 8,
			free:  fleet(gen("p1", "r1", 5), gen("p2", "r1", 3), gen("p2", "r2", 1)),
			want:  sorted(idsOf("p1", "r1", 1, 5), idsOf("p2", "r1", 1, 3)),
		},
		{
			name:  "partial pod without a fitting rack consumes ascending racks",
			count: 8,
			free:  fleet(gen("p1", "r1", 5), gen("p2", "r1", 2), gen("p2", "r2", 2)),
			want:  sorted(idsOf("p1", "r1", 1, 5), idsOf("p2", "r1", 1, 2), idsOf("p2", "r2", 1, 1)),
		},
		{
			name:  "consumes whole multi-rack pods when needed",
			count: 7,
			free:  fleet(gen("p1", "r1", 2), gen("p1", "r2", 3), gen("p2", "r1", 1), gen("p2", "r2", 1)),
			want: sorted(
				idsOf("p1", "r1", 1, 2), idsOf("p1", "r2", 1, 3),
				idsOf("p2", "r1", 1, 1), idsOf("p2", "r2", 1, 1),
			),
		},
	})
}

func TestPlaceDoesNotMutateInput(t *testing.T) {
	free := fleet(
		nodes("p2", "r2", "z", "y"),
		nodes("p1", "r1", "d", "b", "c", "a"),
		nodes("p1", "r2", "e"),
	)
	before := slices.Clone(free)
	for _, policy := range []placement.Policy{placement.PolicySameRack, placement.PolicySamePod, placement.PolicyAny} {
		for count := 1; count <= 8; count++ {
			_, _ = placement.Place(placement.Request{Count: count, Policy: policy}, free)
		}
	}
	if !slices.Equal(free, before) {
		t.Fatalf("input modified: got %v, want %v", free, before)
	}
}

func TestPlaceResultIsIndependentOfInput(t *testing.T) {
	free := gen("p1", "r1", 3)
	req := placement.Request{Count: 2, Policy: placement.PolicySameRack}
	first, err := placement.Place(req, free)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = "tampered"
	second, err := placement.Place(req, free)
	if err != nil {
		t.Fatal(err)
	}
	if want := idsOf("p1", "r1", 1, 2); !slices.Equal(second, want) {
		t.Fatalf("got %v, want %v", second, want)
	}
}
