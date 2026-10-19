package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

// world is the control plane's view of the fleet, which a test edits between
// ticks.
type world struct {
	mu           sync.Mutex
	nodes        []NodeInfo
	reservations []ReservationInfo
	nodesStatus  int
}

func (w *world) set(nodes []NodeInfo, reservations []ReservationInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nodes, w.reservations = nodes, reservations
}

func (w *world) failNodes(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nodesStatus = status
}

func (w *world) serve(rw http.ResponseWriter, c call) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case c.Method == http.MethodGet && c.Path == "/v1/nodes":
		if w.nodesStatus != 0 {
			writeJSONBody(rw, w.nodesStatus, map[string]string{"error": "boom"})
			return
		}
		writeJSONBody(rw, http.StatusOK, map[string]any{"nodes": w.nodes})
	case c.Method == http.MethodGet && c.Path == "/v1/reservations":
		writeJSONBody(rw, http.StatusOK, map[string]any{"reservations": w.reservations})
	case c.Method == http.MethodPost && c.Path == "/v1/reservations":
		writeJSONBody(rw, http.StatusCreated, map[string]string{"id": "res-1"})
	default:
		writeJSONBody(rw, http.StatusOK, map[string]string{})
	}
}

type tickEnv struct {
	rec       *recorder
	world     *world
	svc       *Service
	logs      *bytes.Buffer
	clock     time.Time
	bmcStatus atomic.Int32
	xidStatus atomic.Int32
}

func newTickEnv(t *testing.T) *tickEnv {
	t.Helper()
	env := &tickEnv{rec: &recorder{}, world: &world{}, logs: &bytes.Buffer{}, clock: t0}
	env.bmcStatus.Store(http.StatusNoContent)
	env.xidStatus.Store(http.StatusNoContent)

	cp := newFake(t, "cp", env.rec, env.world.serve)
	bmc := newFake(t, "bmc", env.rec, func(w http.ResponseWriter, c call) {
		if requireBMCAuth(w, c) {
			w.WriteHeader(int(env.bmcStatus.Load()))
		}
	})
	agent := newFake(t, "agent1", env.rec, func(w http.ResponseWriter, _ call) {
		w.WriteHeader(int(env.xidStatus.Load()))
	})
	cfg := Config{
		ControlPlaneURL: cp.URL, BMCSimURL: bmc.URL, NodeBMCURL: bmc.URL,
		BMCUser: testBMCUser, BMCPassword: testBMCPassword,
		Agents: map[string]string{"node-001": agent.URL},
		Seed:   7, Tick: time.Second,
	}
	env.svc = New(cfg, slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		},
	})))
	env.svc.now = func() time.Time { return env.clock }
	return env
}

func (e *tickEnv) tickAt(offset time.Duration) {
	e.clock = t0.Add(offset)
	e.svc.Tick(context.Background())
}

// writes are the calls that change something, without the snapshot reads.
func (e *tickEnv) writes() []string {
	var out []string
	for _, c := range e.rec.all() {
		if c.Method != http.MethodGet {
			out = append(out, c.String())
		}
	}
	return out
}

func (e *tickEnv) logLines() []string {
	text := strings.TrimSpace(e.logs.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestTickRepairsAQuarantinedNodeAfterItWasSeenForTwoMinutes(t *testing.T) {
	env := newTickEnv(t)
	env.world.set([]NodeInfo{nodeOf("node-010", node.StateQuarantined)}, twoOpen())

	env.tickAt(0)
	env.tickAt(repairAfter - time.Second)
	if w := env.writes(); len(w) != 0 {
		t.Fatalf("too early to repair, got %v", w)
	}

	env.tickAt(repairAfter)
	want := []string{"bmc DELETE /sim/systems/node-010/tamper", "cp POST /v1/nodes/node-010/events"}
	if w := env.writes(); !slices.Equal(w, want) {
		t.Fatalf("writes = %v, want %v", w, want)
	}
	if _, ok := env.svc.state.QuarantinedSince["node-010"]; ok {
		t.Error("a repaired node must be forgotten")
	}
	if lines := env.logLines(); len(lines) != 1 || !strings.Contains(lines[0], "repair node-010") {
		t.Errorf("want one log line for the repair, got %q", lines)
	}
}

func TestTickReservesWhenTheFleetIsIdleAndLogsOneLinePerAction(t *testing.T) {
	env := newTickEnv(t)
	env.tickAt(0)
	env.tickAt(10 * time.Second)

	if w := env.writes(); !slices.Equal(w, []string{"cp POST /v1/reservations", "cp POST /v1/reservations"}) {
		t.Fatalf("writes = %v", w)
	}
	var tenants []string
	for _, c := range env.rec.all() {
		if c.Method != http.MethodPost {
			continue
		}
		var body struct {
			Tenant string `json:"tenant"`
		}
		if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, body.Tenant)
	}
	if !slices.Equal(tenants, []string{"tenant-1", "tenant-2"}) {
		t.Errorf("tenants = %v", tenants)
	}
	lines := env.logLines()
	if len(lines) != 2 {
		t.Fatalf("want one line per action, got %q", lines)
	}
	for i, line := range lines {
		if !strings.Contains(line, "kind=reserve") || !strings.Contains(line, "res-1") {
			t.Errorf("line %d: %s", i, line)
		}
	}
}

func TestTickDoesNothingWhenNothingApplies(t *testing.T) {
	env := newTickEnv(t)
	env.world.set(nil, twoOpen())
	env.tickAt(0)
	if w := env.writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	if lines := env.logLines(); len(lines) != 0 {
		t.Errorf("an idle tick logs nothing, got %q", lines)
	}
}

func TestTickSurvivesASnapshotFailure(t *testing.T) {
	env := newTickEnv(t)
	env.world.failNodes(http.StatusInternalServerError)
	env.tickAt(0)
	if w := env.writes(); len(w) != 0 {
		t.Errorf("no snapshot, no action, got %v", w)
	}
	if lines := env.logLines(); len(lines) != 1 || !strings.Contains(lines[0], "snapshot failed") {
		t.Errorf("want one warning, got %q", lines)
	}

	env.world.failNodes(0)
	env.tickAt(10 * time.Second)
	if w := env.writes(); len(w) != 1 {
		t.Errorf("the next tick must work again, got %v", w)
	}
}

func TestTickRetriesAFailedRepairOnTheNextTick(t *testing.T) {
	env := newTickEnv(t)
	env.world.set([]NodeInfo{nodeOf("node-010", node.StateQuarantined)}, twoOpen())
	env.bmcStatus.Store(http.StatusInternalServerError)

	env.tickAt(0)
	env.tickAt(repairAfter)
	env.tickAt(repairAfter + 10*time.Second)
	if w := env.writes(); !slices.Equal(w, []string{
		"bmc DELETE /sim/systems/node-010/tamper",
		"bmc DELETE /sim/systems/node-010/tamper",
	}) {
		t.Fatalf("writes = %v: the failed repair must be retried without sending the event", w)
	}
	if lines := env.logLines(); len(lines) != 2 || !strings.Contains(lines[0], "action failed") {
		t.Errorf("want 2 failure warnings, got %q", lines)
	}

	env.bmcStatus.Store(http.StatusNoContent)
	env.tickAt(repairAfter + 20*time.Second)
	if w := env.writes(); w[len(w)-1] != "cp POST /v1/nodes/node-010/events" {
		t.Errorf("the repair should finish once bmcsim recovers, got %v", w)
	}
}

func TestTickCountsAFailedXIDTowardTheRateButNotTheFaultCap(t *testing.T) {
	env := newTickEnv(t)
	env.world.set([]NodeInfo{nodeOf("node-001", node.StateActive)}, twoOpen())
	env.xidStatus.Store(http.StatusInternalServerError)

	env.tickAt(0)
	if len(env.svc.state.Pending) != 0 {
		t.Fatalf("a failed xid injected nothing: %v", env.svc.state.Pending)
	}
	env.tickAt(xidInterval - time.Second)
	if w := env.writes(); len(w) != 1 {
		t.Fatalf("a failing agent must not be hammered, writes = %v", w)
	}

	env.xidStatus.Store(http.StatusNoContent)
	env.tickAt(xidInterval)
	if w := env.writes(); len(w) != 2 {
		t.Fatalf("after the interval the xid is tried again, writes = %v", w)
	}
	if _, ok := env.svc.state.Pending["node-001"]; !ok {
		t.Error("a successful xid leaves the node pending until it is quarantined")
	}
}

func TestTickPlantsAnImplantInAnActiveReservation(t *testing.T) {
	env := newTickEnv(t)
	env.world.set(
		[]NodeInfo{nodeOf("node-010", node.StateActive)},
		[]ReservationInfo{resOf("r1", ReservationActive, "node-010"), resOf("s", ReservationScheduled)},
	)
	env.tickAt(0)
	w := env.writes()
	if len(w) != 1 || w[0] != "bmc POST /sim/systems/node-010/tamper" {
		t.Fatalf("writes = %v", w)
	}
	env.tickAt(implantInterval - time.Second)
	if w := env.writes(); len(w) != 1 {
		t.Errorf("implants are at least %s apart, writes = %v", implantInterval, w)
	}
}

func TestRunStartsUpThenTicksUntilTheContextIsCancelled(t *testing.T) {
	f := newFleetFake(t, 3)
	s := f.service()
	s.cfg.Tick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, "three ticks", func() bool { return countPath(f.rec, "/v1/nodes", http.MethodGet) >= 3 })
	if specs, _ := f.specs(); len(specs) != 3 {
		t.Errorf("startup registered %d nodes, want 3", len(specs))
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a clean stop is not an error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

func TestRunFailsWhenStartupCannotComplete(t *testing.T) {
	f := newFleetFake(t, 3)
	f.healthFailures = 1 << 30
	s := f.service()
	s.backoff = Backoff{Initial: time.Millisecond, Max: time.Millisecond, Timeout: 20 * time.Millisecond}
	if err := s.Run(context.Background()); err == nil {
		t.Error("want a startup error")
	}
}

func countPath(rec *recorder, path, method string) int {
	n := 0
	for _, c := range rec.all() {
		if c.Path == path && c.Method == method {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
