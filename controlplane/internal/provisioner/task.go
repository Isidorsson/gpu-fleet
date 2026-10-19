package provisioner

import (
	"context"
	"errors"
	"fmt"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/redfish"
)

var errTaskTimeout = errors.New("task timed out")

// faultError marks a failure that means the node itself is unusable: the BMC
// or agent kept failing, answered with a permanent error, or the task never
// finished.
type faultError struct{ err error }

func (e *faultError) Error() string { return e.err.Error() }
func (e *faultError) Unwrap() error { return e.err }

func isTransient(err error) bool {
	return errors.Is(err, redfish.ErrTransient) || errors.Is(err, deployagent.ErrTransient)
}

// runTask boots the node into the agent for task and waits for it to finish,
// returning PhaseSucceeded or PhaseFailed.
func (s *session) runTask(ctx context.Context, task deployagent.Task) (deployagent.Phase, error) {
	if err := s.startTask(ctx, task); err != nil {
		return "", err
	}
	deadline := s.w.clock.Now().Add(s.w.cfg.TaskTimeout)
	for {
		if err := s.w.clock.Sleep(ctx, s.w.cfg.PollInterval); err != nil {
			return "", fmt.Errorf("wait for task %s: %w", task, err)
		}
		if err := s.renew(ctx); err != nil {
			return "", err
		}
		st, err := s.status(ctx)
		if err != nil {
			return "", err
		}
		if st.Task == task && (st.Phase == deployagent.PhaseSucceeded || st.Phase == deployagent.PhaseFailed) {
			return st.Phase, nil
		}
		if !s.w.clock.Now().Before(deadline) {
			return "", &faultError{fmt.Errorf("task %s: %w after %s", task, errTaskTimeout, s.w.cfg.TaskTimeout)}
		}
	}
}

func (s *session) startTask(ctx context.Context, task deployagent.Task) error {
	id := s.n.ID
	steps := []struct {
		op string
		fn func(context.Context) error
	}{
		{"set task", func(ctx context.Context) error { return s.agent.SetTask(ctx, id, task) }},
		{"set pxe boot once", func(ctx context.Context) error { return s.bmc.SetPXEBootOnce(ctx, id) }},
		{"force restart", func(ctx context.Context) error { return s.bmc.Reset(ctx, id, redfish.ResetForceRestart) }},
	}
	for _, step := range steps {
		if err := s.call(ctx, step.op, step.fn); err != nil {
			return err
		}
	}
	return nil
}

// forceOff renews the lease first: powering off is destructive, so it must not
// run on a node another worker may already own.
func (s *session) forceOff(ctx context.Context) error {
	if err := s.renew(ctx); err != nil {
		return err
	}
	return s.call(ctx, "force off", func(ctx context.Context) error {
		return s.bmc.Reset(ctx, s.n.ID, redfish.ResetForceOff)
	})
}

func (s *session) status(ctx context.Context) (deployagent.Status, error) {
	var st deployagent.Status
	err := s.call(ctx, "agent status", func(ctx context.Context) (err error) {
		st, err = s.agent.Status(ctx, s.n.ID)
		return err
	})
	return st, err
}

// call runs fn, retrying transient failures with doubling backoff up to
// MaxAttempts tries in total. The lease is renewed after each backoff so a
// long retry sequence cannot outlive it. Exhausted retries and permanent
// errors come back as a faultError.
func (s *session) call(ctx context.Context, op string, fn func(context.Context) error) error {
	cfg := s.w.cfg
	backoff := cfg.RetryBackoff
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", op, ctx.Err())
		}
		log := s.logger().With("op", op, "attempt", attempt, "error", err)
		if !isTransient(err) {
			log.Error("call failed")
			return &faultError{fmt.Errorf("%s: %w", op, err)}
		}
		if attempt >= cfg.MaxAttempts {
			log.Error("call failed, retries exhausted", "max_attempts", cfg.MaxAttempts)
			return &faultError{fmt.Errorf("%s: gave up after %d attempts: %w", op, attempt, err)}
		}
		log.Warn("call failed, retrying", "backoff", backoff)
		if err := s.w.clock.Sleep(ctx, backoff); err != nil {
			return fmt.Errorf("%s: backoff: %w", op, err)
		}
		if err := s.renew(ctx); err != nil {
			return err
		}
		backoff = min(backoff*2, cfg.leaseHeadroom())
	}
}
