package provisioner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/bmcsim"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
)

// End to end over real HTTP: bmcsim serves the protocol, the real redfish and
// deployagent clients speak it, and several workers share the fleet. This is
// the contract test between the simulator and the clients, which were written
// independently from docs/bmc-protocol.md.

const (
	e2eNodes    = 12
	e2eWorkers  = 3
	e2eUser     = "sim"
	e2ePassword = "sim-test-only"
	e2eDeadline = 20 * time.Second
)

type e2eFleet struct {
	store *fakeStore
	bmc   *redfish.Client
	url   string
}

func newE2EFleet(t *testing.T) *e2eFleet {
	t.Helper()
	handler, err := bmcsim.NewHandler(bmcsim.Config{
		Nodes:        e2eNodes,
		Seed:         42,
		TaskDuration: 20 * time.Millisecond,
		InspectFail:  0.3,
		BMCErrorRate: 0.1,
		User:         e2eUser,
		Password:     e2ePassword,
	}, bmcsim.SystemClock{})
	if err != nil {
		t.Fatalf("bmcsim: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	f := &e2eFleet{
		store: newFakeStore(),
		bmc:   redfish.New(srv.URL, e2eUser, e2ePassword, nil),
		url:   srv.URL,
	}
	for i := range e2eNodes {
		f.store.add(node.Register(bmcsim.NodeID(i), "r1", srv.URL))
	}
	return f
}

func (f *e2eFleet) dial(url string) (BMC, Agent) {
	return redfish.New(url, e2eUser, e2ePassword, nil), deployagent.New(url, e2eUser, e2ePassword, nil)
}

// runUntilSettled runs workers until no node is in a work state.
func (f *e2eFleet) runUntilSettled(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eDeadline)
	defer cancel()

	var wg sync.WaitGroup
	for i := range e2eWorkers {
		w, err := NewWorker(Config{
			Owner:        fmt.Sprintf("worker-%d", i),
			LeaseTTL:     5 * time.Second,
			PollInterval: 5 * time.Millisecond,
			IdleBackoff:  5 * time.Millisecond,
			TaskTimeout:  2 * time.Second,
			MaxAttempts:  8,
			RetryBackoff: 2 * time.Millisecond,
		}, Deps{Store: f.store, Dial: f.dial, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
		wg.Go(func() {
			if err := w.Run(ctx); err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		})
	}

	for !f.settled() {
		if ctx.Err() != nil {
			t.Fatalf("fleet did not settle: %v", f.states())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}

func (f *e2eFleet) settled() bool {
	for _, st := range f.states() {
		if isWorkState(st) {
			return false
		}
	}
	return true
}

func (f *e2eFleet) states() map[string]node.State {
	out := map[string]node.State{}
	for i := range e2eNodes {
		id := bmcsim.NodeID(i)
		out[id] = f.store.get(id).State
	}
	return out
}

// powerState retries through injected 503s so assertions see the real state.
func (f *e2eFleet) powerState(t *testing.T, id string) redfish.PowerState {
	t.Helper()
	for range 20 {
		sys, err := f.bmc.System(t.Context(), id)
		if err == nil {
			return sys.PowerState
		}
	}
	t.Fatalf("system %s unreadable", id)
	return ""
}

func TestE2EFleetInspectionAndProvisioning(t *testing.T) {
	f := newE2EFleet(t)
	f.runUntilSettled(t)

	var available []string
	for id, st := range f.states() {
		switch st {
		case node.StateAvailable:
			available = append(available, id)
		case node.StateQuarantined:
		default:
			t.Errorf("%s settled in %s, want available or quarantined", id, st)
		}
		if p := f.powerState(t, id); st == node.StateAvailable && p != redfish.PowerOff {
			t.Errorf("%s is available but powered %s", id, p)
		}
	}
	if len(available) == 0 || len(available) == e2eNodes {
		t.Fatalf("%d of %d available; seed 42 with 30%% inspect failures should give a mix", len(available), e2eNodes)
	}

	for _, id := range available {
		f.store.mu.Lock()
		n := f.store.nodes[id]
		n.State, n.Tenant = node.StateProvisioning, "acme"
		f.store.nodes[id] = n
		f.store.mu.Unlock()
	}
	f.runUntilSettled(t)

	for _, id := range available {
		n := f.store.get(id)
		if n.State != node.StateActive || n.Tenant != "acme" {
			t.Errorf("%s = %s/%q, want active for acme", id, n.State, n.Tenant)
		}
		if p := f.powerState(t, id); p != redfish.PowerOn {
			t.Errorf("%s is active but powered %s", id, p)
		}
	}
}
