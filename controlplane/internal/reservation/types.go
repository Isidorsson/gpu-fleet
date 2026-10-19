// Package reservation models time-boxed capacity reservations and the
// reconciler that turns them into node lifecycle events. See
// docs/reservations.md.
package reservation

import (
	"errors"
	"fmt"
	"time"

	"gpu-fleet/controlplane/internal/placement"
)

type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusActive    Status = "active"
	StatusDegraded  Status = "degraded"
	StatusEnded     Status = "ended"
	StatusCancelled Status = "cancelled"
)

// Statuses returns every reservation status.
func Statuses() []Status {
	return []Status{StatusScheduled, StatusActive, StatusDegraded, StatusEnded, StatusCancelled}
}

const (
	MaxNodes    = 64
	MaxDuration = 90 * 24 * time.Hour
)

var (
	ErrNotFound = errors.New("reservation not found")
	ErrInvalid  = errors.New("invalid reservation")
	// ErrNotCancellable is returned when cancelling an ended or cancelled
	// reservation.
	ErrNotCancellable = errors.New("reservation cannot be cancelled")
)

// Request asks for Count nodes satisfying Policy for [StartsAt, EndsAt).
type Request struct {
	Tenant   string
	Count    int
	Policy   placement.Policy
	StartsAt time.Time
	EndsAt   time.Time
}

type Reservation struct {
	ID        string
	Tenant    string
	Count     int
	Policy    placement.Policy
	StartsAt  time.Time
	EndsAt    time.Time
	Status    Status
	NodeIDs   []string
	CreatedAt time.Time
}

// Validate checks a request at the API boundary. now clamps a StartsAt in
// the past to now, so "start immediately" needs no special value.
func (r Request) Validate(now time.Time) (Request, error) {
	if r.StartsAt.Before(now) {
		r.StartsAt = now
	}
	switch {
	case r.Tenant == "":
		return r, fmt.Errorf("%w: tenant is required", ErrInvalid)
	case r.Count < 1 || r.Count > MaxNodes:
		return r, fmt.Errorf("%w: count must be 1..%d", ErrInvalid, MaxNodes)
	case !r.EndsAt.After(r.StartsAt):
		return r, fmt.Errorf("%w: ends_at must be after starts_at", ErrInvalid)
	case r.EndsAt.Sub(r.StartsAt) > MaxDuration:
		return r, fmt.Errorf("%w: longer than %s", ErrInvalid, MaxDuration)
	}
	if _, err := placement.ParsePolicy(string(r.Policy)); err != nil {
		return r, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return r, nil
}

// Metrics is what the API and reconciler report. A no-op is used when nil.
type Metrics interface {
	PlacementFailed(policy placement.Policy)
	Degraded()
}

type NopMetrics struct{}

func (NopMetrics) PlacementFailed(placement.Policy) {}
func (NopMetrics) Degraded()                        {}
