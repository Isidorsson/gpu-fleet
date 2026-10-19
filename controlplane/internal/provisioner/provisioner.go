// Package provisioner drives nodes through the lifecycle states that need
// work: inspecting, provisioning and wiping.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

type Store interface {
	ClaimWork(ctx context.Context, owner string, ttl time.Duration) (node.Node, error)
	RenewLease(ctx context.Context, id, owner string, ttl time.Duration) error
	ReleaseLease(ctx context.Context, id, owner string) error
	ApplyEventAsOwner(ctx context.Context, id, owner string, ev node.Event, tenant string) (node.Node, error)
}

type BMC interface {
	SetPXEBootOnce(ctx context.Context, id string) error
	Reset(ctx context.Context, id string, t redfish.ResetType) error
}

type Agent interface {
	SetTask(ctx context.Context, id string, t deployagent.Task) error
	Status(ctx context.Context, id string) (deployagent.Status, error)
}

// Dial returns clients for a node's BMC base URL (node.Node.BMC).
type Dial func(bmcURL string) (BMC, Agent)

var ErrInvalidConfig = errors.New("invalid provisioner config")

// Config tunes a Worker. PollInterval and RetryBackoff are capped at half of
// LeaseTTL so the lease is renewed well before it can lapse.
type Config struct {
	// Owner identifies this worker in lease ownership checks.
	Owner    string
	LeaseTTL time.Duration
	// PollInterval is the gap between agent status polls.
	PollInterval time.Duration
	// IdleBackoff is how long Run sleeps when there is no work or a node failed.
	IdleBackoff time.Duration
	// TaskTimeout is how long a task may run before the node is quarantined.
	TaskTimeout time.Duration
	// MaxAttempts is the total number of tries per BMC or agent call.
	MaxAttempts int
	// RetryBackoff is the first retry delay; it doubles on each retry.
	RetryBackoff time.Duration
}

// leaseHeadroom is the longest the worker may go without renewing its lease.
func (c Config) leaseHeadroom() time.Duration { return c.LeaseTTL / 2 }

func (c Config) validate() error {
	checks := []struct {
		ok  bool
		msg string
	}{
		{c.Owner != "", "Owner is required"},
		{c.LeaseTTL > 0, "LeaseTTL must be positive"},
		{c.PollInterval > 0, "PollInterval must be positive"},
		{c.PollInterval <= c.leaseHeadroom(), "PollInterval must be at most LeaseTTL/2"},
		{c.IdleBackoff > 0, "IdleBackoff must be positive"},
		{c.TaskTimeout > 0, "TaskTimeout must be positive"},
		{c.MaxAttempts >= 1, "MaxAttempts must be at least 1"},
		{c.RetryBackoff > 0, "RetryBackoff must be positive"},
		{c.RetryBackoff <= c.leaseHeadroom(), "RetryBackoff must be at most LeaseTTL/2"},
	}
	var errs []error
	for _, check := range checks {
		if !check.ok {
			errs = append(errs, fmt.Errorf("%w: %s", ErrInvalidConfig, check.msg))
		}
	}
	return errors.Join(errs...)
}

type Deps struct {
	Store Store
	Dial  Dial
	// Clock defaults to the wall clock.
	Clock Clock
	// Log defaults to slog.Default().
	Log *slog.Logger
	// Metrics defaults to discarding everything.
	Metrics Metrics
}

type Worker struct {
	cfg     Config
	store   Store
	dial    Dial
	clock   Clock
	log     *slog.Logger
	metrics Metrics
}

func NewWorker(cfg Config, deps Deps) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if deps.Store == nil || deps.Dial == nil {
		return nil, fmt.Errorf("%w: Store and Dial are required", ErrInvalidConfig)
	}
	w := &Worker{cfg: cfg, store: deps.Store, dial: deps.Dial, clock: deps.Clock, log: deps.Log, metrics: deps.Metrics}
	if w.clock == nil {
		w.clock = wallClock{}
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.metrics == nil {
		w.metrics = noopMetrics{}
	}
	return w, nil
}

// Step claims one node and drives it until it reaches a state that needs no
// work, or something stops it. worked is false when there was nothing to claim.
func (w *Worker) Step(ctx context.Context) (worked bool, err error) {
	n, err := w.store.ClaimWork(ctx, w.cfg.Owner, w.cfg.LeaseTTL)
	if errors.Is(err, store.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim work: %w", err)
	}
	return true, w.newSession(n).run(ctx)
}

// Run steps until ctx is done. A failing node is logged and skipped so one
// bad node cannot stop the worker.
func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		worked, err := w.Step(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("step failed", "owner", w.cfg.Owner, "error", err)
		}
		if worked && err == nil {
			continue
		}
		if w.clock.Sleep(ctx, w.cfg.IdleBackoff) != nil {
			break
		}
	}
	return nil
}
