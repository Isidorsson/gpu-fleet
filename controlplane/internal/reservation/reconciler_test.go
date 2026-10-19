package reservation

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

func TestTickDoesNothingWhenNotLeader(t *testing.T) {
	h := newHarness(t)
	h.leader.leader = false
	h.reserve("res-1", StatusScheduled, "n1")
	h.reserve("res-2", StatusActive, "n2")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.set("n2", node.StateActive, testTenant)
	h.expire()

	h.tick(t)

	assertEvents(t, h)
	assertEqual(t, "due-to-start queries", h.store.dueStarts, 0)
	assertEqual(t, "due-to-end queries", h.store.dueEnds, 0)
	assertEqual(t, "running queries", h.store.runs, 0)
	assertEqual(t, "leader checks", h.leader.calls, 1)
}

func TestTickActsOnceLeadershipIsAcquired(t *testing.T) {
	h := newHarness(t)
	h.leader.leader = false
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.set("n1", node.StateAvailable, "")

	h.tick(t)
	assertEvents(t, h)

	h.leader.leader = true
	h.tick(t)

	assertEvents(t, h, provisioned("n1"))
}

func TestTickReportsLeaderErrorsWithoutActing(t *testing.T) {
	h := newHarness(t)
	h.leader.err = errors.New("lock connection lost")

	err := h.rec.Tick(context.Background())

	if !errors.Is(err, h.leader.err) {
		t.Fatalf("Tick = %v, want the leader error", err)
	}
	assertEqual(t, "due-to-start queries", h.store.dueStarts, 0)
}

func TestTickReportsStoreListErrors(t *testing.T) {
	h := newHarness(t)
	listErr := errors.New("db down")
	h.store.dueStartErrs = []error{listErr}

	if err := h.rec.Tick(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("Tick = %v, want the list error", err)
	}
}

func TestRunStopsWhenTheContextIsCanceled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onSleep = func(count int) {
		if count == 3 {
			cancel()
		}
	}

	if err := h.rec.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil on cancellation", err)
	}

	assertEqual(t, "ticks", h.leader.calls, 3)
	for _, d := range h.clock.sleeps {
		assertEqual(t, "sleep between ticks", d, testTick)
	}
}

func TestRunDoesNotTickWithACanceledContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.rec.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	assertEqual(t, "ticks", h.leader.calls, 0)
}

func TestRunSurvivesATickErrorAndKeepsReconciling(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.set("n1", node.StateAvailable, "")
	h.store.dueStartErrs = []error{errors.New("db down")}
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onSleep = func(count int) {
		if count == 2 {
			cancel()
		}
	}

	if err := h.rec.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	assertEqual(t, "status after recovery", h.status("res-1"), StatusActive)
	assertEvents(t, h, provisioned("n1"))
}

func TestRunSurvivesALeaderErrorAndLogsIt(t *testing.T) {
	h := newHarness(t)
	h.leader.err = errors.New("lock connection lost")
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onSleep = func(count int) {
		if count == 2 {
			cancel()
		}
	}

	if err := h.rec.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if h.logs.Len() == 0 {
		t.Error("leader error was not logged")
	}
}

func TestNewReconcilerValidatesConfigAndDeps(t *testing.T) {
	h := newHarness(t)
	full := Deps{Store: h.store, Nodes: h.nodes, Leader: h.leader, Place: testPlacer}
	tests := []struct {
		name   string
		cfg    Config
		mutate func(*Deps)
	}{
		{"zero tick", Config{LockKey: testLockKey}, nil},
		{"negative tick", Config{Tick: -time.Second, LockKey: testLockKey}, nil},
		{"zero lock key", Config{Tick: testTick}, nil},
		{"no store", testConfig(), func(d *Deps) { d.Store = nil }},
		{"no nodes", testConfig(), func(d *Deps) { d.Nodes = nil }},
		{"no leader", testConfig(), func(d *Deps) { d.Leader = nil }},
		{"no placer", testConfig(), func(d *Deps) { d.Place = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := full
			if tt.mutate != nil {
				tt.mutate(&deps)
			}
			if _, err := NewReconciler(tt.cfg, deps); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewReconciler = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewReconcilerDefaultsOptionalDeps(t *testing.T) {
	h := newHarness(t)
	rec, err := NewReconciler(testConfig(), Deps{Store: h.store, Nodes: h.nodes, Leader: h.leader, Place: testPlacer})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	now := time.Now()
	h.store.add(Reservation{
		ID: "res-1", Tenant: testTenant, Count: 2, Status: StatusScheduled, NodeIDs: []string{"n1", "n2"},
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
	})
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateAvailable, "")

	if err := rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick with default clock, metrics and log: %v", err)
	}
	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
}

func TestDefaultsAreValid(t *testing.T) {
	if err := (Config{Tick: DefaultTick, LockKey: DefaultLockKey}).validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}
