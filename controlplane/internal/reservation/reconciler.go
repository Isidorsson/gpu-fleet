package reservation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

const (
	DefaultTick = 5 * time.Second
	// DefaultLockKey is "gpu_resv" as a big-endian int64; it names the
	// advisory lock that elects the reconciler leader.
	DefaultLockKey int64 = 0x6770755f72657376
)

var ErrInvalidConfig = errors.New("invalid reservation reconciler config")

// Store is the part of the reservation store the reconciler uses.
type Store interface {
	DueToStart(ctx context.Context, now time.Time) ([]Reservation, error)
	DueToEnd(ctx context.Context, now time.Time) ([]Reservation, error)
	SetReservationStatus(ctx context.Context, id string, s Status) error
	ReplaceNode(ctx context.Context, id, oldNodeID string, now time.Time, place placement.Placer) (string, error)
}

// Nodes reads nodes and applies operator-style lifecycle events to them;
// provisioner workers pick the resulting work up.
type Nodes interface {
	Get(ctx context.Context, id string) (node.Node, error)
	ApplyEvent(ctx context.Context, id string, ev node.Event, tenant string) (node.Node, error)
}

// Leader elects the one reconciler that acts. TryAcquire reports whether the
// caller is the leader now; it is called every tick and must not block.
type Leader interface {
	TryAcquire(ctx context.Context) (bool, error)
}

type Config struct {
	// Tick is the gap between reconciliation passes.
	Tick time.Duration
	// LockKey identifies the leader election lock; every control plane
	// instance must use the same key.
	LockKey int64
}

func (c Config) validate() error {
	var errs []error
	if c.Tick <= 0 {
		errs = append(errs, fmt.Errorf("%w: Tick must be positive", ErrInvalidConfig))
	}
	if c.LockKey == 0 {
		errs = append(errs, fmt.Errorf("%w: LockKey is required", ErrInvalidConfig))
	}
	return errors.Join(errs...)
}

type Deps struct {
	Store  Store
	Nodes  Nodes
	Leader Leader
	// Place chooses replacements for quarantined nodes.
	Place placement.Placer
	// Clock defaults to the wall clock.
	Clock Clock
	// Metrics defaults to discarding everything.
	Metrics Metrics
	// Log defaults to slog.Default().
	Log *slog.Logger
}

type Reconciler struct {
	cfg     Config
	store   Store
	nodes   Nodes
	leader  Leader
	place   placement.Placer
	clock   Clock
	metrics Metrics
	log     *slog.Logger
}

func NewReconciler(cfg Config, deps Deps) (*Reconciler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if deps.Store == nil || deps.Nodes == nil || deps.Leader == nil || deps.Place == nil {
		return nil, fmt.Errorf("%w: Store, Nodes, Leader and Place are required", ErrInvalidConfig)
	}
	r := &Reconciler{
		cfg: cfg, store: deps.Store, nodes: deps.Nodes, leader: deps.Leader,
		place: deps.Place, clock: deps.Clock, metrics: deps.Metrics, log: deps.Log,
	}
	if r.clock == nil {
		r.clock = wallClock{}
	}
	if r.metrics == nil {
		r.metrics = NopMetrics{}
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// Tick is one reconciliation pass. A follower does nothing. Errors for single
// reservations are joined so one bad reservation does not hide the others.
func (r *Reconciler) Tick(ctx context.Context) error {
	leader, err := r.leader.TryAcquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire leadership: %w", err)
	}
	if !leader {
		return nil
	}
	now := r.clock.Now()
	// Ending first frees a tenant's nodes before a back-to-back reservation
	// for the same tenant looks at them.
	endErr := r.endDue(ctx, now)
	startErr := r.startDue(ctx, now)
	return errors.Join(endErr, startErr)
}

// Run ticks until ctx is done. Failures are logged and retried on the next
// tick, so Run returns nil on cancellation.
func (r *Reconciler) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("reservation tick failed", "error", err)
		}
		if r.clock.Sleep(ctx, r.cfg.Tick) != nil {
			break
		}
	}
	return nil
}

func (r *Reconciler) startDue(ctx context.Context, now time.Time) error {
	due, err := r.store.DueToStart(ctx, now)
	if err != nil {
		return fmt.Errorf("list reservations due to start: %w", err)
	}
	return forEach(ctx, due, "start", func(res Reservation) error { return r.start(ctx, res, now) })
}

func (r *Reconciler) endDue(ctx context.Context, now time.Time) error {
	due, err := r.store.DueToEnd(ctx, now)
	if err != nil {
		return fmt.Errorf("list reservations due to end: %w", err)
	}
	return forEach(ctx, due, "end", func(res Reservation) error { return r.end(ctx, res) })
}

func forEach(ctx context.Context, due []Reservation, op string, fn func(Reservation) error) error {
	var errs []error
	for _, res := range due {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := fn(res); err != nil {
			errs = append(errs, fmt.Errorf("%s reservation %s: %w", op, res.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) setStatus(ctx context.Context, id string, s Status) error {
	if err := r.store.SetReservationStatus(ctx, id, s); err != nil {
		return fmt.Errorf("set status %s: %w", s, err)
	}
	return nil
}
