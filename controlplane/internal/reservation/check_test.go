package reservation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpu-fleet/controlplane/internal/node"
)

func TestCheckLeavesAHealthyReservationAlone(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1", "n2")
	h.nodes.hold("n1", node.StateActive, "res-1")
	h.nodes.hold("n2", node.StateProvisioning, "res-1")

	h.tick(t)

	assertEvents(t, h)
	assertEqual(t, "status", h.status("res-1"), StatusActive)
	assertEqual(t, "replace calls", len(h.store.replaceCalls), 0)
	assertEqual(t, "status writes", len(h.store.statusLog), 0)
}

func TestCheckReplacesANodeThatLeftMidRun(t *testing.T) {
	tests := []struct {
		name  string
		state node.State
	}{
		{"quarantined", node.StateQuarantined},
		{"wiping after provisioning failed", node.StateWiping},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.reserve("res-1", StatusActive, "n1", "n2")
			h.nodes.hold("n1", node.StateActive, "res-1")
			h.nodes.hold("n2", tt.state, "res-1")
			h.nodes.set("n3", node.StateAvailable, "")
			h.store.replacements["n2"] = "n3"

			h.tick(t)

			assertEqual(t, "status", h.status("res-1"), StatusDegraded)
			assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
			if len(h.store.replaceCalls) != 1 {
				t.Fatalf("ReplaceNode calls = %v, want 1", h.store.replaceCalls)
			}
			assertEqual(t, "replaced node", h.store.replaceCalls[0].OldNode, "n2")
			assertEqual(t, "replace time", h.store.replaceCalls[0].Now, testStart)
			assertEvents(t, h, provisioned("n3"))
			assertEqual(t, "replacement holder", h.nodes.get("n3").Reservation, "res-1")

			h.tick(t)

			assertEvents(t, h, provisioned("n3"))
			assertEqual(t, "degraded count after another tick", h.metrics.degradedCount(), 1)
			assertEqual(t, "replace calls after another tick", len(h.store.replaceCalls), 1)
		})
	}
}

func TestCheckDropsALostNodeWhenNothingFits(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1", "n2")
	h.nodes.hold("n1", node.StateActive, "res-1")
	h.nodes.hold("n2", node.StateQuarantined, "res-1")

	h.tick(t)

	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
	assertEqual(t, "dropped", strings.Join(h.store.drops, ","), "n2")
	assertEqual(t, "nodes left", strings.Join(h.store.get("res-1").NodeIDs, ","), "n1")

	h.tick(t)
	assertEqual(t, "replace calls after another tick", len(h.store.replaceCalls), 1)
	assertEqual(t, "degraded count after another tick", h.metrics.degradedCount(), 1)
}

func TestCheckCountsDegradedOnceForSeveralLostNodes(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1", "n2", "n3")
	h.nodes.hold("n1", node.StateQuarantined, "res-1")
	h.nodes.hold("n2", node.StateWiping, "res-1")
	h.nodes.hold("n3", node.StateActive, "res-1")

	h.tick(t)

	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
	assertEqual(t, "status writes", strings.Join(h.store.statusLog, ","), "res-1=degraded")
	assertEqual(t, "dropped", strings.Join(h.store.drops, ","), "n1,n2")
}

func TestCheckDoesNotCountAnAlreadyDegradedReservation(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusDegraded, "n1")
	h.nodes.hold("n1", node.StateQuarantined, "res-1")

	h.tick(t)

	assertEqual(t, "degraded count", h.metrics.degradedCount(), 0)
	assertEqual(t, "status writes", len(h.store.statusLog), 0)
	assertEqual(t, "dropped", strings.Join(h.store.drops, ","), "n1")
}

func TestCheckWaitsForAReplacementThatIsNotReady(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusDegraded, "n1", "n2")
	h.nodes.hold("n1", node.StateActive, "res-1")
	h.nodes.set("n2", node.StateWiping, otherTenant)

	h.tick(t)
	assertEvents(t, h)
	assertEqual(t, "replace calls", len(h.store.replaceCalls), 0)

	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)

	assertEvents(t, h, provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
}

func TestCheckRetriesWhenTheDegradedStatusIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.nodes.hold("n1", node.StateQuarantined, "res-1")
	h.nodes.set("n2", node.StateAvailable, "")
	h.store.replacements["n1"] = "n2"
	h.store.statusErrs = []error{errors.New("db down")}

	if err := h.rec.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("Tick = %v, want the status write error", err)
	}
	assertEqual(t, "degraded count after failed write", h.metrics.degradedCount(), 0)
	assertEqual(t, "replace calls after failed write", len(h.store.replaceCalls), 0)

	h.tick(t)

	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
	assertEvents(t, h, provisioned("n2"))
}

func TestCheckReplaceErrorIsReportedAndRetried(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.nodes.hold("n1", node.StateQuarantined, "res-1")
	h.store.replaceErrs["n1"] = errors.New("db down")

	err := h.rec.Tick(context.Background())

	if err == nil || !strings.Contains(err.Error(), "check reservation res-1") {
		t.Fatalf("Tick = %v, want the replace error naming res-1", err)
	}
	assertEqual(t, "dropped", len(h.store.drops), 0)
	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
}
