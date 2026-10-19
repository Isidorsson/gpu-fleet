package metrics

import (
	"context"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/provisioner"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

// A real provisioner.Worker feeding the real registry, so a label value the
// adapter rejects (wrong arity, bad name) fails here instead of in production.

const (
	workerPoll    = 2 * time.Second
	workerBackoff = 100 * time.Millisecond
)

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

type oneNodeStore struct {
	n       node.Node
	claimed bool
}

func (s *oneNodeStore) ClaimWork(context.Context, string, time.Duration) (node.Node, error) {
	if s.claimed {
		return node.Node{}, store.ErrNoWork
	}
	s.claimed = true
	return s.n, nil
}

func (s *oneNodeStore) RenewLease(context.Context, string, string, time.Duration) error { return nil }
func (s *oneNodeStore) ReleaseLease(context.Context, string, string) error              { return nil }

func (s *oneNodeStore) ApplyEventAsOwner(_ context.Context, _, _ string, ev node.Event, tenant string) (node.Node, error) {
	next, err := s.n.Apply(ev, tenant)
	if err == nil {
		s.n = next
	}
	return next, err
}

// machine is a BMC and agent whose first PXE call fails transiently and whose
// task reports running once before it succeeds.
type machine struct {
	task     deployagent.Task
	pxeCalls int
	polls    int
}

func (m *machine) SetTask(_ context.Context, _ string, t deployagent.Task) error {
	m.task = t
	return nil
}

func (m *machine) SetPXEBootOnce(context.Context, string) error {
	m.pxeCalls++
	if m.pxeCalls == 1 {
		return redfish.ErrTransient
	}
	return nil
}

func (m *machine) Reset(context.Context, string, redfish.ResetType) error { return nil }

func (m *machine) Status(context.Context, string) (deployagent.Status, error) {
	m.polls++
	phase := deployagent.PhaseRunning
	if m.polls > 1 {
		phase = deployagent.PhaseSucceeded
	}
	return deployagent.Status{Task: m.task, Phase: phase}, nil
}

func TestWorkerFeedsTheRegistry(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	mach := &machine{}
	w, err := provisioner.NewWorker(provisioner.Config{
		Owner:        "worker-1",
		LeaseTTL:     30 * time.Second,
		PollInterval: workerPoll,
		IdleBackoff:  5 * time.Second,
		TaskTimeout:  time.Minute,
		MaxAttempts:  3,
		RetryBackoff: workerBackoff,
	}, provisioner.Deps{
		Store:   &oneNodeStore{n: node.Register(node.Spec{ID: "n1", Pod: "p1", Rack: "r1", BMC: "http://bmc.test"})},
		Dial:    func(string) (provisioner.BMC, provisioner.Agent) { return mach, mach },
		Clock:   &stepClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		Metrics: m.Provisioner(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	if worked, err := w.Step(context.Background()); !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}

	gatherAndCompare(t, m, family("provisioner_tasks_total",
		"Tasks run by provisioner workers, by task and outcome.", "counter",
		`provisioner_tasks_total{outcome="succeeded",task="inspect"} 1`,
	), "provisioner_tasks_total")
	gatherAndCompare(t, m, family("provisioner_bmc_requests_total",
		"BMC and agent call attempts, retries included.", "counter",
		`provisioner_bmc_requests_total{op="reset",result="ok"} 2`,
		`provisioner_bmc_requests_total{op="set_pxe",result="ok"} 1`,
		`provisioner_bmc_requests_total{op="set_pxe",result="transient"} 1`,
		`provisioner_bmc_requests_total{op="set_task",result="ok"} 1`,
		`provisioner_bmc_requests_total{op="status",result="ok"} 2`,
	), "provisioner_bmc_requests_total")
	gatherAndCompare(t, m, family("provisioner_events_total",
		"Lifecycle events applied by provisioner workers.", "counter",
		`provisioner_events_total{event="hardware_fault"} 0`,
		`provisioner_events_total{event="inspection_failed"} 0`,
		`provisioner_events_total{event="inspection_passed"} 1`,
		`provisioner_events_total{event="start_inspection"} 1`,
		`provisioner_events_total{event="wipe_failed"} 0`,
	), "provisioner_events_total")
	gatherAndCompare(t, m, family("provisioner_task_duration_seconds",
		"Time from the start of a task to its result, in seconds.", "histogram",
		`provisioner_task_duration_seconds_bucket{task="inspect",le="1"} 0`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="5"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="15"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="30"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="60"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="120"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="300"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="600"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="1200"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="+Inf"} 1`,
		`provisioner_task_duration_seconds_sum{task="inspect"} 4.1`,
		`provisioner_task_duration_seconds_count{task="inspect"} 1`,
	), "provisioner_task_duration_seconds")
}
