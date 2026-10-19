package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

func TestRunStopsCleanlyOnCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.clock.onSleep = func(count int, _ time.Duration) {
		if count == 3 {
			cancel()
		}
	}

	if err := h.worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	assertEqual(t, "claims", h.store.claims, 3)
	assertEqual(t, "idle sleeps", h.clock.sleepsOf(testIdle), 3)
}

func TestRunReturnsImmediatelyWhenAlreadyCanceled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	assertEqual(t, "claims", h.store.claims, 0)
}

func TestRunSurvivesPerNodeError(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"n1", "n2"} {
		n := nodeIn(node.StateInspecting, "")
		n.ID = id
		h.store.add(n)
	}
	h.store.applyErr = errors.New("db down")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.clock.onSleep = func(_ int, d time.Duration) {
		if d == testIdle && h.clock.sleepsOf(testIdle) == 3 {
			cancel()
		}
	}

	if err := h.worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	for _, id := range []string{"n1", "n2"} {
		assertEqual(t, id+" state", h.store.get(id).State, node.StateAvailable)
		assertEqual(t, id+" lease", h.store.holder(id), "")
	}
	if !strings.Contains(h.logs.String(), "db down") {
		t.Errorf("per-node error was not logged:\n%s", h.logs.String())
	}
}

func TestRunKeepsGoingAfterClaimError(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.store.claimErr = errors.New("db down")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.clock.onSleep = func(_ int, d time.Duration) {
		if d == testIdle && h.clock.sleepsOf(testIdle) == 2 {
			cancel()
		}
	}

	if err := h.worker.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateAvailable)
}
