package provisioner

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
)

const (
	testOwner    = "worker-1"
	testNodeID   = "n1"
	testBMCURL   = "http://bmc.test"
	testPoll     = 2 * time.Second
	testIdle     = 5 * time.Second
	testTimeout  = 60 * time.Second
	testBackoff  = 100 * time.Millisecond
	testAttempts = 3
)

func testConfig() Config {
	return Config{
		Owner:        testOwner,
		LeaseTTL:     30 * time.Second,
		PollInterval: testPoll,
		IdleBackoff:  testIdle,
		TaskTimeout:  testTimeout,
		MaxAttempts:  testAttempts,
		RetryBackoff: testBackoff,
	}
}

type harness struct {
	store    *fakeStore
	machine  *fakeMachine
	attester *fakeAttester
	clock    *fakeClock
	logs     *bytes.Buffer
	metrics  *recordingMetrics
	worker   *Worker
	start    time.Time
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{
		store:   newFakeStore(),
		machine: newFakeMachine(),
		clock:   newFakeClock(),
		logs:    &bytes.Buffer{},
		metrics: &recordingMetrics{},
	}
	h.attester = newFakeAttester(h.machine)
	h.start = h.clock.Now()
	cfg := testConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, err := NewWorker(cfg, Deps{
		Store: h.store, Dial: h.machine.dial, Attester: h.attester,
		Clock: h.clock, Log: log, Metrics: h.metrics,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	h.worker = w
	return h
}

func (h *harness) step(t *testing.T) {
	t.Helper()
	worked, err := h.worker.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
}

func (h *harness) backoffs() []time.Duration {
	return h.clock.sleepsExcept(testPoll, testIdle)
}

func nodeIn(st node.State, tenant string) node.Node {
	return node.Node{ID: testNodeID, Rack: "r1", BMC: testBMCURL, State: st, Tenant: tenant}
}

func taskCalls(task deployagent.Task, powerOff bool) []string {
	calls := []string{"set_task:" + string(task), "pxe_once", "reset:ForceRestart"}
	if powerOff {
		calls = append(calls, "reset:ForceOff")
	}
	return calls
}

// attestedInspectCalls is taskCalls for an inspection that the agent reported
// as succeeded: the node is attested before it is powered off.
func attestedInspectCalls() []string {
	return append(taskCalls(deployagent.TaskInspect, false), "attest", "reset:ForceOff")
}

func assertSlice[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func assertEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func assertReleasedOnce(t *testing.T, h *harness) {
	t.Helper()
	assertSlice(t, "releases", h.store.releases, []string{testNodeID})
	assertEqual(t, "lease holder", h.store.holder(testNodeID), "")
}
