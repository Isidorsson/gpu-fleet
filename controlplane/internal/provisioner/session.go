package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/store"
)

const releaseTimeout = 5 * time.Second

// plan describes how a work state is processed. The machine is powered off
// after an inspection or wipe so it never idles in the ramdisk, but a
// provisioned machine is left running for its tenant.
type plan struct {
	task          deployagent.Task
	success       node.Event
	failure       node.Event
	powerOffAfter bool
}

var plans = map[node.State]plan{
	node.StateInspecting: {
		task:          deployagent.TaskInspect,
		success:       node.EventInspectionPassed,
		failure:       node.EventInspectionFailed,
		powerOffAfter: true,
	},
	node.StateProvisioning: {
		task:    deployagent.TaskDeploy,
		success: node.EventProvisionSucceeded,
		failure: node.EventProvisionFailed,
	},
	node.StateWiping: {
		task:          deployagent.TaskWipe,
		success:       node.EventWipeSucceeded,
		failure:       node.EventWipeFailed,
		powerOffAfter: true,
	},
}

func isWorkState(s node.State) bool {
	_, ok := plans[s]
	return ok || s == node.StateRacked
}

// session is one worker's hold on one claimed node. It is used by a single
// goroutine.
type session struct {
	w     *Worker
	n     node.Node
	bmc   BMC
	agent Agent
	task  deployagent.Task
}

func (w *Worker) newSession(n node.Node) *session {
	bmc, agent := w.dial(n.BMC)
	return &session{w: w, n: n, bmc: bmc, agent: agent}
}

func (s *session) logger() *slog.Logger {
	l := s.w.log.With("node", s.n.ID, "state", s.n.State)
	if s.task != "" {
		l = l.With("task", s.task)
	}
	return l
}

// run drives the node and releases the lease unless it was lost. Losing the
// lease or being overruled by an operator is a normal outcome, not an error.
func (s *session) run(ctx context.Context) error {
	err := s.drive(ctx)
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		s.logger().Warn("lease lost, abandoning node", "error", err)
		s.w.metrics.LeaseLost()
		return nil
	case errors.Is(err, node.ErrInvalidTransition):
		s.logger().Warn("node changed under us, operator wins", "error", err)
		err = nil
	}
	return errors.Join(err, s.release(ctx))
}

func (s *session) drive(ctx context.Context) error {
	if !isWorkState(s.n.State) {
		s.logger().Warn("claimed node needs no work")
	}
	for isWorkState(s.n.State) {
		if err := s.advance(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) advance(ctx context.Context) error {
	s.task = ""
	if s.n.State == node.StateRacked {
		return s.apply(ctx, node.EventStartInspection)
	}
	return s.execute(ctx, plans[s.n.State])
}

// execute runs one task and records it, timed from the first call to the
// applied event.
func (s *session) execute(ctx context.Context, p plan) error {
	s.task = p.task
	start := s.w.clock.Now()
	outcome, err := s.process(ctx, p)
	s.w.metrics.TaskFinished(p.task, outcome, s.w.clock.Now().Sub(start))
	return err
}

func (s *session) process(ctx context.Context, p plan) (Outcome, error) {
	phase, err := s.runTask(ctx, p.task)
	if err == nil {
		s.logger().Info("task finished", "phase", phase)
	}
	if err == nil && p.powerOffAfter {
		err = s.forceOff(ctx)
	}
	if err != nil {
		return s.quarantineOnFault(ctx, err)
	}
	if phase == deployagent.PhaseFailed {
		return s.settle(ctx, p.failure, OutcomeFailed)
	}
	return s.settle(ctx, p.success, OutcomeSucceeded)
}

// quarantineOnFault turns a node fault into hardware_fault. Anything else
// (lease lost, shutdown, store errors) says nothing about the node and is
// returned as is.
func (s *session) quarantineOnFault(ctx context.Context, err error) (Outcome, error) {
	var fault *faultError
	if !errors.As(err, &fault) {
		return OutcomeAbandoned, err
	}
	s.logger().Error("node fault, quarantining", "event", node.EventHardwareFault, "error", err)
	return s.settle(ctx, node.EventHardwareFault, OutcomeFault)
}

// settle applies the event that ends a task. If it cannot be applied the
// task counts as abandoned, whatever the agent reported.
func (s *session) settle(ctx context.Context, ev node.Event, outcome Outcome) (Outcome, error) {
	if err := s.apply(ctx, ev); err != nil {
		return OutcomeAbandoned, err
	}
	return outcome, nil
}

func (s *session) apply(ctx context.Context, ev node.Event) error {
	next, err := s.w.store.ApplyEventAsOwner(ctx, s.n.ID, s.w.cfg.Owner, ev, "")
	if err != nil {
		return fmt.Errorf("apply %s to node %s: %w", ev, s.n.ID, err)
	}
	s.logger().Info("event applied", "event", ev, "to", next.State)
	s.w.metrics.EventApplied(ev)
	s.n = next
	return nil
}

func (s *session) renew(ctx context.Context) error {
	err := s.w.store.RenewLease(ctx, s.n.ID, s.w.cfg.Owner, s.w.cfg.LeaseTTL)
	if err != nil {
		return fmt.Errorf("renew lease for node %s: %w", s.n.ID, err)
	}
	return nil
}

// release uses a fresh context so the lease is given back even when ctx was
// canceled, which is how a shutdown ends a session.
func (s *session) release(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := s.w.store.ReleaseLease(ctx, s.n.ID, s.w.cfg.Owner); err != nil {
		return fmt.Errorf("release lease for node %s: %w", s.n.ID, err)
	}
	return nil
}
