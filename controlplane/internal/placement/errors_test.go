package placement_test

import (
	"errors"
	"testing"

	"gpu-fleet/controlplane/internal/placement"
)

func TestPlaceCapacityError(t *testing.T) {
	cases := []struct {
		name    string
		policy  placement.Policy
		count   int
		free    []placement.Candidate
		largest int
	}{
		{
			name:    "same_rack reports largest rack",
			policy:  placement.PolicySameRack,
			count:   5,
			free:    fleet(gen("p1", "r1", 3), gen("p1", "r2", 2), gen("p2", "r1", 3)),
			largest: 3,
		},
		{
			name:    "same_rack does not merge equal rack names across pods",
			policy:  placement.PolicySameRack,
			count:   3,
			free:    fleet(gen("p1", "r1", 2), gen("p2", "r1", 2)),
			largest: 2,
		},
		{
			name:   "same_rack with nothing free",
			policy: placement.PolicySameRack,
			count:  1,
		},
		{
			name:    "same_pod reports largest pod",
			policy:  placement.PolicySamePod,
			count:   5,
			free:    fleet(gen("p1", "r1", 2), gen("p1", "r2", 2), gen("p2", "r1", 3)),
			largest: 4,
		},
		{
			name:    "same_pod does not merge pods",
			policy:  placement.PolicySamePod,
			count:   4,
			free:    fleet(gen("p1", "r1", 3), gen("p2", "r1", 3)),
			largest: 3,
		},
		{
			name:   "same_pod with nothing free",
			policy: placement.PolicySamePod,
			count:  1,
		},
		{
			name:    "any reports total free",
			policy:  placement.PolicyAny,
			count:   8,
			free:    fleet(gen("p1", "r1", 4), gen("p2", "r1", 3)),
			largest: 7,
		},
		{
			name:    "any one short of the request",
			policy:  placement.PolicyAny,
			count:   3,
			free:    gen("p1", "r1", 2),
			largest: 2,
		},
		{
			name:   "any with nothing free",
			policy: placement.PolicyAny,
			count:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placement.Place(placement.Request{Count: tc.count, Policy: tc.policy}, tc.free)
			if got != nil {
				t.Fatalf("got ids %v, want none", got)
			}
			if !errors.Is(err, placement.ErrInsufficientCapacity) {
				t.Fatalf("err = %v, want ErrInsufficientCapacity", err)
			}
			if errors.Is(err, placement.ErrInvalidRequest) {
				t.Fatalf("err = %v must not match ErrInvalidRequest", err)
			}
			var capErr *placement.CapacityError
			if !errors.As(err, &capErr) {
				t.Fatalf("err = %v, want *CapacityError", err)
			}
			want := placement.CapacityError{Policy: tc.policy, Requested: tc.count, Largest: tc.largest}
			if *capErr != want {
				t.Fatalf("CapacityError = %+v, want %+v", *capErr, want)
			}
		})
	}
}

func TestPlaceInvalidRequest(t *testing.T) {
	valid := gen("p1", "r1", 3)
	cases := []struct {
		name string
		req  placement.Request
		free []placement.Candidate
	}{
		{"count zero", placement.Request{Count: 0, Policy: placement.PolicyAny}, valid},
		{"count negative", placement.Request{Count: -3, Policy: placement.PolicySameRack}, valid},
		{"unknown policy", placement.Request{Count: 1, Policy: "nearby"}, valid},
		{"empty policy", placement.Request{Count: 1}, valid},
		{
			"duplicate ID in one rack",
			placement.Request{Count: 1, Policy: placement.PolicyAny},
			fleet(valid, nodes("p1", "r1", "p1-r1-02")),
		},
		{
			"duplicate ID across pods",
			placement.Request{Count: 1, Policy: placement.PolicySamePod},
			fleet(valid, nodes("p2", "r1", "p1-r1-01")),
		},
		{
			"empty pod",
			placement.Request{Count: 1, Policy: placement.PolicySameRack},
			fleet(valid, nodes("", "r1", "x")),
		},
		{
			"empty rack",
			placement.Request{Count: 1, Policy: placement.PolicySameRack},
			fleet(valid, nodes("p1", "", "x")),
		},
		{
			"empty node ID",
			placement.Request{Count: 1, Policy: placement.PolicyAny},
			fleet(valid, nodes("p1", "r1", "")),
		},
		{
			"invalid input wins over insufficient capacity",
			placement.Request{Count: 99, Policy: placement.PolicyAny},
			fleet(valid, nodes("p1", "r1", "p1-r1-01")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placement.Place(tc.req, tc.free)
			if got != nil {
				t.Fatalf("got ids %v, want none", got)
			}
			if !errors.Is(err, placement.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if errors.Is(err, placement.ErrInsufficientCapacity) {
				t.Fatalf("err = %v must not match ErrInsufficientCapacity", err)
			}
		})
	}
}
