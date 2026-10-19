package remediation

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type Config struct {
	// Limit caps remediation quarantines; the zero value means DefaultLimit.
	Limit Limit
}

type Deps struct {
	Store Store
	// Clock defaults to the wall clock.
	Clock Clock
	// Metrics defaults to discarding everything.
	Metrics Metrics
	// Log defaults to slog.Default().
	Log *slog.Logger
}

type Service struct {
	limit   Limit
	store   Store
	clock   Clock
	metrics Metrics
	log     *slog.Logger
}

func New(cfg Config, deps Deps) (*Service, error) {
	limit := cfg.Limit
	if limit == (Limit{}) {
		limit = DefaultLimit
	}
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: Store is required", ErrInvalidConfig)
	}
	s := &Service{limit: limit, store: deps.Store, clock: deps.Clock, metrics: deps.Metrics, log: deps.Log}
	if s.clock == nil {
		s.clock = wallClock{}
	}
	if s.metrics == nil {
		s.metrics = NopMetrics{}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Handle remediates every alert of w in order and counts the outcomes,
// ignored alerts included. It stops at the first store error, returning the
// summary of the alerts handled before it: Alertmanager retries the whole
// webhook, and the store treats the repeats as already quarantined.
func (s *Service) Handle(ctx context.Context, w Webhook) (Summary, error) {
	if err := checkVersion(w); err != nil {
		return Summary{}, err
	}
	sum := Summary{Outcomes: map[Outcome]int{}}
	for _, a := range w.Alerts {
		outcome, err := s.handleAlert(ctx, a)
		if err != nil {
			return sum, err
		}
		sum.Outcomes[outcome]++
		s.metrics.Remediated(outcome)
	}
	return sum, nil
}

func (s *Service) handleAlert(ctx context.Context, a Alert) (Outcome, error) {
	d, outcome := Decide(a)
	if d.Action == "" {
		return outcome, nil
	}
	outcome, err := s.store.ApplyRemediation(ctx, d, s.limit, s.clock.Now())
	if err != nil {
		return "", fmt.Errorf("remediate node %s for %s: %w", d.NodeID, d.AlertName, err)
	}
	s.logOutcome(d, outcome)
	return outcome, nil
}

func (s *Service) logOutcome(d Decision, outcome Outcome) {
	attrs := []any{"node_id", d.NodeID, "alertname", d.AlertName, "fingerprint", d.Fingerprint}
	switch outcome {
	case OutcomeQuarantined:
		s.log.Info("node quarantined by remediation", attrs...)
	case OutcomeRateLimited:
		s.log.Warn("remediation rate limited, node left in service",
			append(attrs, "max", s.limit.Max, "window", s.limit.Window)...)
	}
}
