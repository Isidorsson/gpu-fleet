package provisioner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
)

const (
	running   = deployagent.PhaseRunning
	succeeded = deployagent.PhaseSucceeded
	failed    = deployagent.PhaseFailed
)

func TestStepScenarios(t *testing.T) {
	tests := []struct {
		name       string
		start      node.Node
		scripts    map[deployagent.Task][]deployagent.Phase
		wantState  node.State
		wantTenant string
		wantEvents []node.Event
		wantCalls  []string
	}{
		{
			name:       "racked node is inspected and becomes available",
			start:      node.Register(testNodeID, "r1", testBMCURL),
			scripts:    map[deployagent.Task][]deployagent.Phase{deployagent.TaskInspect: {running, succeeded}},
			wantState:  node.StateAvailable,
			wantEvents: []node.Event{node.EventStartInspection, node.EventInspectionPassed},
			wantCalls:  taskCalls(deployagent.TaskInspect, true),
		},
		{
			name:       "failed inspection quarantines",
			start:      nodeIn(node.StateInspecting, ""),
			scripts:    map[deployagent.Task][]deployagent.Phase{deployagent.TaskInspect: {running, failed}},
			wantState:  node.StateQuarantined,
			wantEvents: []node.Event{node.EventInspectionFailed},
			wantCalls:  taskCalls(deployagent.TaskInspect, true),
		},
		{
			name:       "provisioning succeeds, keeps tenant and stays powered on",
			start:      nodeIn(node.StateProvisioning, "acme"),
			scripts:    map[deployagent.Task][]deployagent.Phase{deployagent.TaskDeploy: {running, running, succeeded}},
			wantState:  node.StateActive,
			wantTenant: "acme",
			wantEvents: []node.Event{node.EventProvisionSucceeded},
			wantCalls:  taskCalls(deployagent.TaskDeploy, false),
		},
		{
			name:  "failed provisioning wipes, re-inspects and frees the node",
			start: nodeIn(node.StateProvisioning, "acme"),
			scripts: map[deployagent.Task][]deployagent.Phase{
				deployagent.TaskDeploy:  {failed},
				deployagent.TaskWipe:    {running, succeeded},
				deployagent.TaskInspect: {succeeded},
			},
			wantState: node.StateAvailable,
			wantEvents: []node.Event{
				node.EventProvisionFailed, node.EventWipeSucceeded, node.EventInspectionPassed,
			},
			wantCalls: slices.Concat(
				taskCalls(deployagent.TaskDeploy, false),
				taskCalls(deployagent.TaskWipe, true),
				taskCalls(deployagent.TaskInspect, true),
			),
		},
		{
			name:       "failed wipe quarantines and keeps tenant",
			start:      nodeIn(node.StateWiping, "acme"),
			scripts:    map[deployagent.Task][]deployagent.Phase{deployagent.TaskWipe: {failed}},
			wantState:  node.StateQuarantined,
			wantTenant: "acme",
			wantEvents: []node.Event{node.EventWipeFailed},
			wantCalls:  taskCalls(deployagent.TaskWipe, true),
		},
		{
			name:  "inspection after a wipe can still quarantine",
			start: nodeIn(node.StateWiping, "acme"),
			scripts: map[deployagent.Task][]deployagent.Phase{
				deployagent.TaskWipe:    {succeeded},
				deployagent.TaskInspect: {failed},
			},
			wantState:  node.StateQuarantined,
			wantEvents: []node.Event{node.EventWipeSucceeded, node.EventInspectionFailed},
			wantCalls: slices.Concat(
				taskCalls(deployagent.TaskWipe, true),
				taskCalls(deployagent.TaskInspect, true),
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(tc.start)
			for task, phases := range tc.scripts {
				h.machine.script(task, phases...)
			}

			h.step(t)

			got := h.store.get(testNodeID)
			assertEqual(t, "state", got.State, tc.wantState)
			assertEqual(t, "tenant", got.Tenant, tc.wantTenant)
			assertSlice(t, "events", h.store.appliedEvents(), tc.wantEvents)
			assertSlice(t, "bmc calls", h.machine.controlCalls(), tc.wantCalls)
			assertSlice(t, "dialed", h.machine.dialed, []string{testBMCURL})
			assertEqual(t, "claims", h.store.claims, 1)
			assertReleasedOnce(t, h)
		})
	}
}

func TestStepRenewsLeaseOnEveryPoll(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateProvisioning, "acme"))
	h.machine.script(deployagent.TaskDeploy, running, running, succeeded)

	h.step(t)

	assertEqual(t, "renews", h.store.renews, 3)
	assertEqual(t, "status polls", h.machine.count("status"), 3)
	assertEqual(t, "poll sleeps", h.clock.sleepsOf(testPoll), 3)
}

func TestStepNoWork(t *testing.T) {
	h := newHarness(t)

	worked, err := h.worker.Step(context.Background())

	if worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (false, nil)", worked, err)
	}
	assertSlice(t, "bmc calls", h.machine.callLog(), nil)
	assertSlice(t, "releases", h.store.releases, nil)
}

func TestStepClaimError(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("db down")
	h.store.claimErr = boom

	worked, err := h.worker.Step(context.Background())

	if worked || !errors.Is(err, boom) {
		t.Fatalf("Step = (%v, %v), want (false, db down)", worked, err)
	}
}

func TestLogsCarryNodeStateTaskEventAttemptAndError(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.failNext("pxe_once", redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient)

	h.step(t)

	out := h.logs.String()
	for _, want := range []string{
		"node=n1", "state=inspecting", "task=inspect", "attempt=3",
		"event=hardware_fault", "error=", "redfish: transient failure",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q:\n%s", want, out)
		}
	}
}
