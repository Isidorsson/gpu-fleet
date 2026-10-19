package store

import "errors"

var (
	ErrNotFound = errors.New("node not found")
	ErrConflict = errors.New("node already exists")
	// ErrNoWork means no node currently needs a worker.
	ErrNoWork = errors.New("no claimable work")
	// ErrLeaseLost means the caller no longer holds the node's lease, so its
	// result must be discarded.
	ErrLeaseLost = errors.New("lease lost")
	// ErrPlacementContention means every attempt to place a reservation lost
	// the race for its nodes to a concurrent request. Retrying may succeed.
	ErrPlacementContention = errors.New("placement contention")
	// ErrBadPlacement means the placer returned nodes that were not among the
	// free candidates it was given, or the wrong number of them.
	ErrBadPlacement = errors.New("placer returned an invalid placement")
)
