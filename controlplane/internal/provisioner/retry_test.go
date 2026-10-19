package provisioner

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
)

func TestTransientErrorsAreRetriedWithDoublingBackoff(t *testing.T) {
	tests := []struct {
		call string
		err  error
	}{
		{"set_task:inspect", deployagent.ErrTransient},
		{"pxe_once", redfish.ErrTransient},
		{"reset:ForceRestart", fmt.Errorf("post: %w", redfish.ErrTransient)},
		{"status", fmt.Errorf("get: %w", deployagent.ErrTransient)},
		{"attest", fmt.Errorf("fetch evidence: %w", deployagent.ErrTransient)},
		{"reset:ForceOff", redfish.ErrTransient},
	}
	for _, tc := range tests {
		t.Run(tc.call, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(nodeIn(node.StateInspecting, ""))
			h.machine.script(deployagent.TaskInspect, succeeded)
			h.machine.failNext(tc.call, tc.err, tc.err)

			h.step(t)

			assertEqual(t, "attempts", h.machine.count(tc.call), 3)
			assertSlice(t, "backoffs", h.backoffs(), []time.Duration{testBackoff, 2 * testBackoff})
			assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
			assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventInspectionPassed})
			assertReleasedOnce(t, h)
		})
	}
}

func TestBackoffIsCappedAtLeaseHeadroom(t *testing.T) {
	const poll = 300 * time.Millisecond
	h := newHarness(t, func(c *Config) {
		c.LeaseTTL = time.Second
		c.PollInterval = poll
		c.MaxAttempts = 6
	})
	h.store.add(nodeIn(node.StateInspecting, ""))
	transient := []error{redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient}
	h.machine.failNext("pxe_once", transient...)

	h.step(t)

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond}
	assertSlice(t, "backoffs", h.clock.sleepsExcept(poll, testIdle), want)
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
}

func TestExhaustedRetriesQuarantine(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.failNext("pxe_once", redfish.ErrTransient, redfish.ErrTransient, redfish.ErrTransient)

	h.step(t)

	assertEqual(t, "attempts", h.machine.count("pxe_once"), testAttempts)
	assertEqual(t, "restarts", h.machine.count("reset:ForceRestart"), 0)
	assertSlice(t, "backoffs", h.backoffs(), []time.Duration{testBackoff, 2 * testBackoff})
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
	assertReleasedOnce(t, h)
}

func TestNonTransientErrorQuarantinesImmediately(t *testing.T) {
	tests := []struct {
		call string
		err  error
	}{
		{"set_task:inspect", deployagent.ErrUnauthorized},
		{"pxe_once", redfish.ErrNotFound},
		{"reset:ForceRestart", errors.New("boom")},
		{"status", deployagent.ErrNotFound},
		{"attest", errors.New("boom")},
		{"reset:ForceOff", redfish.ErrUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.call, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(nodeIn(node.StateInspecting, ""))
			h.machine.script(deployagent.TaskInspect, succeeded)
			h.machine.failNext(tc.call, tc.err)

			h.step(t)

			assertEqual(t, "attempts", h.machine.count(tc.call), 1)
			assertSlice(t, "backoffs", h.backoffs(), nil)
			assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
			assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
			assertReleasedOnce(t, h)
		})
	}
}

func TestFaultKeepsTenantOfProvisioningNode(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateProvisioning, "acme"))
	h.machine.failNext("set_task:deploy", deployagent.ErrNotFound)

	h.step(t)

	got := h.store.get(testNodeID)
	assertEqual(t, "state", got.State, node.StateQuarantined)
	assertEqual(t, "tenant", got.Tenant, "acme")
}

func TestTaskTimeoutQuarantines(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, running)

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
	assertEqual(t, "elapsed", h.clock.Now().Sub(h.start), testTimeout)
	assertEqual(t, "status polls", h.machine.count("status"), int(testTimeout/testPoll))
	assertReleasedOnce(t, h)
}

func TestIdleAgentCountsAsStillWaiting(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, deployagent.PhaseIdle, deployagent.PhaseIdle, running, succeeded)

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
	assertEqual(t, "status polls", h.machine.count("status"), 4)
}

func TestResultOfAnotherTaskIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.statusFn = func() deployagent.Status {
		return deployagent.Status{Task: deployagent.TaskWipe, Phase: succeeded}
	}

	h.step(t)

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
	assertSlice(t, "events", h.store.appliedEvents(), []node.Event{node.EventHardwareFault})
}
