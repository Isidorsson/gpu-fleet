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
)
