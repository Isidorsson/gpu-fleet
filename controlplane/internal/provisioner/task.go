package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
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
		op   BMCOp
		name string
		fn   func(context.Context) error
	}{
		{OpSetTask, "set task", func(ctx context.Context) error { return s.agent.SetTask(ctx, id, task) }},
		{OpSetPXE, "set pxe boot once", func(ctx context.Context) error { return s.bmc.SetPXEBootOnce(ctx, id) }},
		{OpReset, "force restart", func(ctx context.Context) error { return s.bmc.Reset(ctx, id, redfish.ResetForceRestart) }},
	}
	for _, step := range steps {
		if err := s.call(ctx, step.op, step.name, step.fn); err != nil {
			return err
		}
	}
	return nil
}

// attestNode verifies the node while its ramdisk is still running. It renews
// the lease first because the verdict decides whether the node reaches the
// pool, and the power off that follows renews it again. A verdict other than
// pass or fail cannot be trusted, so it quarantines the node like a failed
// call: the check fails closed.
func (s *session) attestNode(ctx context.Context, p plan) (ending, error) {
	if err := s.renew(ctx); err != nil {
		return ending{}, err
	}
	res, err := s.attestCall(ctx)
	if err != nil {
		return ending{}, err
	}
	switch res.Verdict {
	case attest.VerdictPass, attest.VerdictFail:
	default:
		return ending{}, &faultError{fmt.Errorf("attest: unknown verdict %q", res.Verdict)}
	}
	s.w.metrics.Attested(res.Verdict)
	if res.Verdict == attest.VerdictFail {
		s.logger().Warn("attestation failed", "reasons", strings.Join(res.Reasons, "; "), "reference_version", res.ReferenceVersion)
		return ending{node.EventAttestationFailed, OutcomeFailed}, nil
	}
	s.logger().Info("attestation passed", "reference_version", res.ReferenceVersion)
	return ending{p.success, OutcomeSucceeded}, nil
}

func (s *session) attestCall(ctx context.Context) (attest.Result, error) {
	var res attest.Result
	err := s.call(ctx, OpAttest, "attest", func(ctx context.Context) (err error) {
		res, err = s.w.attester.Attest(ctx, s.n)
		return err
	})
	return res, err
}

// forceOff renews the lease first: powering off is destructive, so it must not
// run on a node another worker may already own.
func (s *session) forceOff(ctx context.Context) error {
	if err := s.renew(ctx); err != nil {
		return err
	}
	return s.call(ctx, OpReset, "force off", func(ctx context.Context) error {
		return s.bmc.Reset(ctx, s.n.ID, redfish.ResetForceOff)
	})
}

func (s *session) status(ctx context.Context) (deployagent.Status, error) {
	var st deployagent.Status
	err := s.call(ctx, OpStatus, "agent status", func(ctx context.Context) (err error) {
		st, err = s.agent.Status(ctx, s.n.ID)
		return err
	})
	return st, err
}

// call runs fn, retrying transient failures with doubling backoff up to
// MaxAttempts tries in total. The lease is renewed after each backoff so a
// long retry sequence cannot outlive it. Exhausted retries and permanent
// errors come back as a faultError. Every attempt is reported to Metrics under
// op, except one cut short by ctx: a shutdown says nothing about the BMC. name
// is for logs and errors.
func (s *session) call(ctx context.Context, op BMCOp, name string, fn func(context.Context) error) error {
	cfg := s.w.cfg
	backoff := cfg.RetryBackoff
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err != nil && ctx.Err() != nil {
			return fmt.Errorf("%s: %w", name, ctx.Err())
		}
		s.w.metrics.BMCRequest(op, resultOf(err))
		if err == nil {
			return nil
		}
		log := s.logger().With("op", name, "attempt", attempt, "error", err)
		if !isTransient(err) {
			log.Error("call failed")
			return &faultError{fmt.Errorf("%s: %w", name, err)}
		}
		if attempt >= cfg.MaxAttempts {
			log.Error("call failed, retries exhausted", "max_attempts", cfg.MaxAttempts)
			return &faultError{fmt.Errorf("%s: gave up after %d attempts: %w", name, attempt, err)}
		}
		log.Warn("call failed, retrying", "backoff", backoff)
		if err := s.w.clock.Sleep(ctx, backoff); err != nil {
			return fmt.Errorf("%s: backoff: %w", name, err)
		}
		if err := s.renew(ctx); err != nil {
			return err
		}
		backoff = min(backoff*2, cfg.leaseHeadroom())
	}
}
