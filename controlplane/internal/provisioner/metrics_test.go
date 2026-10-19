package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

type taskRecord struct {
	Task     deployagent.Task
	Outcome  Outcome
	Duration time.Duration
}

type bmcRecord struct {
	Op     BMCOp
	Result CallResult
}

type recordingMetrics struct {
	mu        sync.Mutex
	tasks     []taskRecord
	bmc       []bmcRecord
	events    []node.Event
	leaseLost int
}

func (r *recordingMetrics) TaskFinished(task deployagent.Task, outcome Outcome, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, taskRecord{task, outcome, d})
}

func (r *recordingMetrics) BMCRequest(op BMCOp, result CallResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bmc = append(r.bmc, bmcRecord{op, result})
}

func (r *recordingMetrics) EventApplied(ev node.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingMetrics) LeaseLost() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseLost++
}

func (r *recordingMetrics) countBMC(want bmcRecord) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, got := range r.bmc {
		if got == want {
			n++
		}
	}
	return n
}

// opOf maps a fakeMachine call string to the BMCOp it should be reported as.
func opOf(call string) BMCOp {
	switch {
	case call == "pxe_once":
		return OpSetPXE
	case call == "status":
		return OpStatus
	case strings.HasPrefix(call, "reset:"):
		return OpReset
	default:
		return OpSetTask
	}
}

func TestTaskOutcomesAndDurations(t *testing.T) {
	tests := []struct {
		name          string
		start         node.Node
		setup         func(h *harness)
		wantTasks     []taskRecord
		wantEvents    []node.Event
		wantLeaseLost int
	}{
		{
			name:       "agent success",
			start:      nodeIn(node.StateInspecting, ""),
			setup:      func(h *harness) { h.machine.script(deployagent.TaskInspect, running, succeeded) },
			wantTasks:  []taskRecord{{deployagent.TaskInspect, OutcomeSucceeded, 2 * testPoll}},
			wantEvents: []node.Event{node.EventInspectionPassed},
		},
		{
			name:       "agent failure",
			start:      nodeIn(node.StateInspecting, ""),
			setup:      func(h *harness) { h.machine.script(deployagent.TaskInspect, running, failed) },
			wantTasks:  []taskRecord{{deployagent.TaskInspect, OutcomeFailed, 2 * testPoll}},
			wantEvents: []node.Event{node.EventInspectionFailed},
		},
		{
			name:       "deploy success takes three polls",
			start:      nodeIn(node.StateProvisioning, "acme"),
			setup:      func(h *harness) { h.machine.script(deployagent.TaskDeploy, running, running, succeeded) },
			wantTasks:  []taskRecord{{deployagent.TaskDeploy, OutcomeSucceeded, 3 * testPoll}},
			wantEvents: []node.Event{node.EventProvisionSucceeded},
		},
		{
			name:  "retries exhausted is a fault and includes the backoffs",
			start: nodeIn(node.StateInspecting, ""),
			setup: func(h *harness) {
				h.machine.failNext("pxe_once", redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient)
			},
			wantTasks:  []taskRecord{{deployagent.TaskInspect, OutcomeFault, 3 * testBackoff}},
			wantEvents: []node.Event{node.EventHardwareFault},
		},
		{
			name:       "non-transient error is a fault",
			start:      nodeIn(node.StateWiping, "acme"),
			setup:      func(h *harness) { h.machine.failNext("set_task:wipe", deployagent.ErrUnauthorized) },
			wantTasks:  []taskRecord{{deployagent.TaskWipe, OutcomeFault, 0}},
			wantEvents: []node.Event{node.EventHardwareFault},
		},
		{
			name:       "timeout is a fault lasting the whole timeout",
			start:      nodeIn(node.StateInspecting, ""),
			setup:      func(h *harness) { h.machine.script(deployagent.TaskInspect, running) },
			wantTasks:  []taskRecord{{deployagent.TaskInspect, OutcomeFault, testTimeout}},
			wantEvents: []node.Event{node.EventHardwareFault},
		},
		{
			name:  "power-off failure after agent success is a fault",
			start: nodeIn(node.StateInspecting, ""),
			setup: func(h *harness) {
				h.machine.script(deployagent.TaskInspect, succeeded)
				h.machine.failNext("reset:ForceOff", redfish.ErrNotFound)
			},
			wantTasks:  []taskRecord{{deployagent.TaskInspect, OutcomeFault, testPoll}},
			wantEvents: []node.Event{node.EventHardwareFault},
		},
		{
			name:  "lease lost while polling is abandoned",
			start: nodeIn(node.StateInspecting, ""),
			setup: func(h *harness) {
				h.machine.script(deployagent.TaskInspect, running)
				h.store.onRenew = func(call int) {
					if call == 2 {
						h.store.loseLease(testNodeID)
					}
				}
			},
			wantTasks:     []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, 2 * testPoll}},
			wantLeaseLost: 1,
		},
		{
			name:          "lease lost on apply is abandoned even though the agent succeeded",
			start:         nodeIn(node.StateInspecting, ""),
			setup:         func(h *harness) { h.store.applyErr = store.ErrLeaseLost },
			wantTasks:     []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, testPoll}},
			wantLeaseLost: 1,
		},
		{
			name:  "operator changing the node makes the result moot",
			start: nodeIn(node.StateInspecting, ""),
			setup: func(h *harness) {
				h.machine.script(deployagent.TaskInspect, running, succeeded)
				h.store.onRenew = func(call int) {
					if call == 1 {
						h.store.operatorApply(testNodeID, node.EventHardwareFault)
					}
				}
			},
			wantTasks: []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, 2 * testPoll}},
		},
		{
			name:  "operator changing the node makes the fault moot",
			start: nodeIn(node.StateInspecting, ""),
			setup: func(h *harness) {
				h.machine.failNext("status", deployagent.ErrNotFound)
				h.store.onRenew = func(call int) {
					if call == 1 {
						h.store.operatorApply(testNodeID, node.EventHardwareFault)
					}
				}
			},
			wantTasks: []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, testPoll}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(tc.start)
			tc.setup(h)

			h.step(t)

			assertSlice(t, "tasks", h.metrics.tasks, tc.wantTasks)
			assertSlice(t, "events", h.metrics.events, tc.wantEvents)
			assertEqual(t, "lease lost", h.metrics.leaseLost, tc.wantLeaseLost)
		})
	}
}

func TestFailedProvisionChainRecordsEveryTaskAndEvent(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateProvisioning, "acme"))
	h.machine.script(deployagent.TaskDeploy, failed)
	h.machine.script(deployagent.TaskWipe, running, succeeded)
	h.machine.script(deployagent.TaskInspect, succeeded)

	h.step(t)

	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{
		{deployagent.TaskDeploy, OutcomeFailed, testPoll},
		{deployagent.TaskWipe, OutcomeSucceeded, 2 * testPoll},
		{deployagent.TaskInspect, OutcomeSucceeded, testPoll},
	})
	assertSlice(t, "events", h.metrics.events, []node.Event{
		node.EventProvisionFailed, node.EventWipeSucceeded, node.EventInspectionPassed,
	})
}

func TestRackedNodeCountsStartInspectionButNoTaskForIt(t *testing.T) {
	h := newHarness(t)
	h.store.add(node.Register(testNodeID, "r1", testBMCURL))

	h.step(t)

	assertSlice(t, "events", h.metrics.events, []node.Event{node.EventStartInspection, node.EventInspectionPassed})
	assertEqual(t, "tasks", len(h.metrics.tasks), 1)
}

func TestEveryCallAttemptIsCountedByOp(t *testing.T) {
	tests := []struct {
		call string
		op   BMCOp
		err  error
	}{
		{"set_task:inspect", OpSetTask, deployagent.ErrTransient},
		{"pxe_once", OpSetPXE, redfish.ErrTransient},
		{"reset:ForceRestart", OpReset, fmt.Errorf("post: %w", redfish.ErrTransient)},
		{"status", OpStatus, fmt.Errorf("get: %w", deployagent.ErrTransient)},
		{"reset:ForceOff", OpReset, redfish.ErrTransient},
	}
	for _, tc := range tests {
		t.Run(tc.call, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(nodeIn(node.StateInspecting, ""))
			h.machine.script(deployagent.TaskInspect, succeeded)
			h.machine.failNext(tc.call, tc.err, tc.err)

			h.step(t)

			var wantOps []BMCOp
			for _, c := range h.machine.callLog() {
				wantOps = append(wantOps, opOf(c))
			}
			var gotOps []BMCOp
			for _, b := range h.metrics.bmc {
				gotOps = append(gotOps, b.Op)
			}
			assertSlice(t, "ops, one per attempt", gotOps, wantOps)
			assertEqual(t, "transient", h.metrics.countBMC(bmcRecord{tc.op, ResultTransient}), 2)
			assertEqual(t, "error", h.metrics.countBMC(bmcRecord{tc.op, ResultError}), 0)
			okTotal := 0
			for _, op := range []BMCOp{OpSetTask, OpSetPXE, OpReset, OpStatus} {
				okTotal += h.metrics.countBMC(bmcRecord{op, ResultOK})
			}
			assertEqual(t, "ok", okTotal, len(wantOps)-2)
		})
	}
}

func TestTransientRetriesThenSuccessSequence(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateProvisioning, "acme"))
	h.machine.script(deployagent.TaskDeploy, succeeded)
	h.machine.failNext("pxe_once", redfish.ErrTransient, redfish.ErrTransient)

	h.step(t)

	assertSlice(t, "bmc", h.metrics.bmc, []bmcRecord{
		{OpSetTask, ResultOK},
		{OpSetPXE, ResultTransient},
		{OpSetPXE, ResultTransient},
		{OpSetPXE, ResultOK},
		{OpReset, ResultOK},
		{OpStatus, ResultOK},
	})
}

func TestPermanentErrorIsCountedOnceAsError(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.failNext("reset:ForceRestart", errors.New("boom"))

	h.step(t)

	assertSlice(t, "bmc", h.metrics.bmc, []bmcRecord{
		{OpSetTask, ResultOK},
		{OpSetPXE, ResultOK},
		{OpReset, ResultError},
	})
}

type cancelOnPXE struct {
	BMC
	cancel context.CancelFunc
}

func (c cancelOnPXE) SetPXEBootOnce(ctx context.Context, _ string) error {
	c.cancel()
	return ctx.Err()
}

func TestAttemptCutShortByShutdownIsNotCounted(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.worker.dial = func(url string) (BMC, Agent) {
		bmc, agent := h.machine.dial(url)
		return cancelOnPXE{BMC: bmc, cancel: cancel}, agent
	}

	_, err := h.worker.Step(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Step error = %v, want context canceled", err)
	}
	assertSlice(t, "bmc", h.metrics.bmc, []bmcRecord{{OpSetTask, ResultOK}})
	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, 0}})
}

func TestShutdownMidTaskIsAbandonedNotLeaseLost(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, running)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.clock.onSleep = func(count int, _ time.Duration) {
		if count == 2 {
			cancel()
		}
	}

	if _, err := h.worker.Step(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Step error = %v, want context canceled", err)
	}

	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, 2 * testPoll}})
	assertEqual(t, "lease lost", h.metrics.leaseLost, 0)
}

func TestStoreFailureOnApplyIsAbandonedNotLeaseLost(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.store.applyErr = errors.New("db down")

	if _, err := h.worker.Step(context.Background()); err == nil {
		t.Fatal("Step error = nil, want db down")
	}

	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, testPoll}})
	assertEqual(t, "lease lost", h.metrics.leaseLost, 0)
	assertEqual(t, "events", len(h.metrics.events), 0)
}

func TestLeaseLostBeforeFirstTaskStillCounts(t *testing.T) {
	h := newHarness(t)
	h.store.add(node.Register(testNodeID, "r1", testBMCURL))
	h.store.applyErr = store.ErrLeaseLost

	h.step(t)

	assertEqual(t, "lease lost", h.metrics.leaseLost, 1)
	assertEqual(t, "tasks", len(h.metrics.tasks), 0)
}

func TestWorkerWithoutMetricsDiscardsThem(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	w, err := NewWorker(testConfig(), Deps{Store: h.store, Dial: h.machine.dial, Clock: h.clock})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	worked, err := w.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
}
