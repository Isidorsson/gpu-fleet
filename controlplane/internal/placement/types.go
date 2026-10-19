// Package placement chooses which nodes satisfy a capacity request. It is
// pure: callers supply the free nodes. Rules are in docs/reservations.md.
package placement

import (
	"errors"
	"fmt"
)

type Policy string

const (
	PolicySameRack Policy = "same_rack"
	PolicySamePod  Policy = "same_pod"
	PolicyAny      Policy = "any"
)

// Candidate is a node that is free for the whole requested window.
type Candidate struct {
	NodeID string
	Pod    string
	Rack   string
}

type Request struct {
	Count  int
	Policy Policy
}

// PodCapacity summarizes free nodes in one pod.
type PodCapacity struct {
	Pod         string
	Free        int
	LargestRack int
}

var (
	ErrInvalidRequest = errors.New("invalid placement request")
	// ErrInsufficientCapacity is matched by every *CapacityError.
	ErrInsufficientCapacity = errors.New("insufficient capacity")
)

// CapacityError says why a request cannot be placed: Largest is the biggest
// block that would satisfy Policy right now.
type CapacityError struct {
	Policy    Policy
	Requested int
	Largest   int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("cannot place %d nodes with policy %s: largest block available is %d",
		e.Requested, e.Policy, e.Largest)
}

func (e *CapacityError) Is(target error) bool { return target == ErrInsufficientCapacity }

// Placer is the signature of Place, so callers such as the store can take it
// as a dependency and tests can substitute it.
type Placer func(req Request, free []Candidate) ([]string, error)

func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(s); p {
	case PolicySameRack, PolicySamePod, PolicyAny:
		return p, nil
	}
	return "", fmt.Errorf("%w: unknown policy %q", ErrInvalidRequest, s)
}
