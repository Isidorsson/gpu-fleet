package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
)

const (
	reasonBootloader = "bootloader digest not in reference values"
	reasonVBIOS      = "gpu 3 vbios not allowed"
	referenceVersion = "2026-10-19.1"
)

func failResult(reasons ...string) attest.Result {
	return attest.Result{
		NodeID: testNodeID, Verdict: attest.VerdictFail, Reasons: reasons, ReferenceVersion: referenceVersion,
	}
}

func inspectingHarness(t *testing.T, phases ...deployagent.Phase) *harness {
	t.Helper()
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, phases...)
	return h
}

func TestPassingAttestationPowersOffThenAppliesInspectionPassed(t *testing.T) {
	h := inspectingHarness(t, running, succeeded)
	var callsWhenResultApplied []string
	h.store.onApply = func(node.Event) { callsWhenResultApplied = h.machine.callLog() }

	h.step(t)

	assertSlice(t, "calls before the result event", callsWhenResultApplied, []string{
		"set_task:inspect", "pxe_once", "reset:ForceRestart", "status", "status", "attest", "reset:ForceOff",
	})
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventInspectionPassed})
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
	assertSlice(t, "attested", h.metrics.attested, []attest.Verdict{attest.VerdictPass})
	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeSucceeded, 2 * testPoll}})
	assertReleasedOnce(t, h)
}

func TestAttesterGetsTheClaimedNodeInInspectingState(t *testing.T) {
	h := newHarness(t)
	h.store.add(node.Register(node.Spec{ID: testNodeID, Pod: "p1", Rack: "r1", BMC: testBMCURL}))

	h.step(t)

	assertEqual(t, "attest calls", len(h.attester.nodes), 1)
	got := h.attester.nodes[0]
	assertEqual(t, "node id", got.ID, testNodeID)
	assertEqual(t, "node state", got.State, node.StateInspecting)
}

func TestFailedAttestationQuarantinesAfterPowerOffAndLogsReasons(t *testing.T) {
	h := inspectingHarness(t, running, succeeded)
	h.attester.queue(failResult(reasonBootloader, reasonVBIOS))

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventAttestationFailed})
	assertSlice(t, "bmc calls", h.machine.controlCalls(), attestedInspectCalls())
	assertSlice(t, "attested", h.metrics.attested, []attest.Verdict{attest.VerdictFail})
	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeFailed, 2 * testPoll}})
	assertSlice(t, "metric events", h.metrics.events, []node.Event{node.EventAttestationFailed})
	for _, want := range []string{"attestation failed", reasonBootloader, reasonVBIOS, referenceVersion, "node=n1"} {
		if !strings.Contains(h.logs.String(), want) {
			t.Errorf("log output missing %q:\n%s", want, h.logs.String())
		}
	}
	assertReleasedOnce(t, h)
}

func TestTransientAttestFailuresAreRetriedAndCountedOnce(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	transient := fmt.Errorf("fetch evidence: %w", deployagent.ErrTransient)
	h.machine.failNext("attest", transient, transient)

	h.step(t)

	assertEqual(t, "attempts", h.machine.count("attest"), 3)
	assertSlice(t, "backoffs", h.backoffs(), []time.Duration{testBackoff, 2 * testBackoff})
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventInspectionPassed})
	assertSlice(t, "attested", h.metrics.attested, []attest.Verdict{attest.VerdictPass})
	assertEqual(t, "transient", h.metrics.countBMC(bmcRecord{OpAttest, ResultTransient}), 2)
	assertEqual(t, "ok", h.metrics.countBMC(bmcRecord{OpAttest, ResultOK}), 1)
}

func TestExhaustedAttestRetriesAreAHardwareFault(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	transient := fmt.Errorf("fetch evidence: %w", deployagent.ErrTransient)
	h.machine.failNext("attest", transient, transient, transient)

	h.step(t)

	assertEqual(t, "attempts", h.machine.count("attest"), testAttempts)
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
	assertSlice(t, "attested", h.metrics.attested, nil)
	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeFault, testPoll + 3*testBackoff}})
	assertEqual(t, "transient", h.metrics.countBMC(bmcRecord{OpAttest, ResultTransient}), testAttempts)
	assertReleasedOnce(t, h)
}

func TestNonTransientAttestErrorIsAHardwareFaultWithoutRetry(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	h.machine.failNext("attest", errors.New("signing key unavailable"))

	h.step(t)

	assertEqual(t, "attempts", h.machine.count("attest"), 1)
	assertSlice(t, "backoffs", h.backoffs(), nil)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
	assertSlice(t, "attested", h.metrics.attested, nil)
	assertEqual(t, "error", h.metrics.countBMC(bmcRecord{OpAttest, ResultError}), 1)
	if !strings.Contains(h.logs.String(), "signing key unavailable") {
		t.Errorf("fault cause was not logged:\n%s", h.logs.String())
	}
}

func TestUnknownVerdictFailsClosed(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	h.attester.queue(attest.Result{NodeID: testNodeID, Verdict: "maybe"})

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
	assertSlice(t, "attested", h.metrics.attested, nil)
}

func TestFailedInspectTaskSkipsAttestation(t *testing.T) {
	h := inspectingHarness(t, running, failed)

	h.step(t)

	assertEqual(t, "attest calls", h.machine.count("attest"), 0)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventInspectionFailed})
	assertSlice(t, "attested", h.metrics.attested, nil)
	assertEqual(t, "attest requests", h.metrics.countBMC(bmcRecord{OpAttest, ResultOK}), 0)
}

func TestInspectTaskTimeoutSkipsAttestation(t *testing.T) {
	h := inspectingHarness(t, running)

	h.step(t)

	assertEqual(t, "attest calls", h.machine.count("attest"), 0)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
}

func TestDeployAndFailedWipeAreNotAttested(t *testing.T) {
	tests := []struct {
		name  string
		start node.Node
		task  deployagent.Task
		phase deployagent.Phase
		want  []node.Event
	}{
		{"deploy", nodeIn(node.StateProvisioning, "acme"), deployagent.TaskDeploy, succeeded, []node.Event{node.EventProvisionSucceeded}},
		{"failed wipe", nodeIn(node.StateWiping, "acme"), deployagent.TaskWipe, failed, []node.Event{node.EventWipeFailed}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(tc.start)
			h.machine.script(tc.task, tc.phase)

			h.step(t)

			assertEqual(t, "attest calls", h.machine.count("attest"), 0)
			assertSlice(t, "events", h.store.appliedEvents(), tc.want)
		})
	}
}

func TestInspectionAfterWipeIsAttestedBeforeTheNodeReturnsToThePool(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateWiping, "acme"))
	h.machine.script(deployagent.TaskWipe, succeeded)
	h.machine.script(deployagent.TaskInspect, succeeded)
	h.attester.queue(failResult(reasonBootloader))

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventWipeSucceeded, node.EventAttestationFailed})
	assertEqual(t, "attest calls", h.machine.count("attest"), 1)
	assertEqual(t, "attested node state", h.attester.nodes[0].State, node.StateInspecting)
}

func TestLeaseIsRenewedBeforeAndAfterAttestation(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	var renewsAtAttest int
	h.attester.onAttest = func() { renewsAtAttest = h.store.renews }

	h.step(t)

	assertEqual(t, "renews before attest", renewsAtAttest, 2)
	assertEqual(t, "renews in total", h.store.renews, 3)
}

func TestLeaseLostBeforeAttestationSkipsIt(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	h.store.onRenew = func(call int) {
		if call == 2 {
			h.store.loseLease(testNodeID)
		}
	}

	h.step(t)

	assertEqual(t, "attest calls", h.machine.count("attest"), 0)
	assertEqual(t, "force offs", h.machine.count("reset:ForceOff"), 0)
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertEqual(t, "lease lost", h.metrics.leaseLost, 1)
}

func TestLeaseLostDuringAttestationAbandonsNode(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	h.attester.onAttest = func() { h.store.loseLease(testNodeID) }

	worked, err := h.worker.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertEqual(t, "force offs", h.machine.count("reset:ForceOff"), 0)
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateInspecting)
	assertSlice(t, "releases", h.store.releases, nil)
	assertEqual(t, "lease lost", h.metrics.leaseLost, 1)
	assertSlice(t, "tasks", h.metrics.tasks, []taskRecord{{deployagent.TaskInspect, OutcomeAbandoned, testPoll}})
}

func TestLeaseLostWhileRetryingAttestationAbandonsNode(t *testing.T) {
	h := inspectingHarness(t, succeeded)
	h.machine.failNext("attest", deployagent.ErrTransient)
	h.attester.onAttest = func() { h.store.loseLease(testNodeID) }

	h.step(t)

	assertEqual(t, "attempts", h.machine.count("attest"), 1)
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertEqual(t, "lease lost", h.metrics.leaseLost, 1)
}
