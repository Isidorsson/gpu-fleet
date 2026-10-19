package reservation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

const (
	testTenant   = "acme"
	otherTenant  = "globex"
	testTick     = 5 * time.Second
	testLockKey  = 42
	placerMarker = "placer-from-deps"
)

var testStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeClock advances instantly: Sleep adds to Now and never blocks.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(count int)
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	count, hook := len(c.sleeps), c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(count)
	}
	return ctx.Err()
}

type replaceCall struct {
	ReservationID string
	OldNode       string
	Now           time.Time
	PlacerResult  []string
}

// fakeStore keeps reservations in memory with the due-ness rules of the real
// store. Errors are queued and consumed one per call.
type fakeStore struct {
	mu           sync.Mutex
	nodes        *fakeNodes
	order        []string
	byID         map[string]*Reservation
	replacements map[string]string
	replaceErrs  map[string]error
	replaceCalls []replaceCall
	drops        []string
	statusLog    []string
	statusErrs   []error
	dueStartErrs []error
	dueStarts    int
	runs         int
	dueEnds      int
}

func newFakeStore(nodes *fakeNodes) *fakeStore {
	return &fakeStore{
		nodes:        nodes,
		byID:         map[string]*Reservation{},
		replacements: map[string]string{},
		replaceErrs:  map[string]error{},
	}
}

func (s *fakeStore) add(res Reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, res.ID)
	s.byID[res.ID] = &res
}

func (s *fakeStore) get(id string) Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.byID[id]
}

func (s *fakeStore) due(match func(Reservation) bool) []Reservation {
	var out []Reservation
	for _, id := range s.order {
		if res := *s.byID[id]; match(res) {
			res.NodeIDs = append([]string(nil), res.NodeIDs...)
			out = append(out, res)
		}
	}
	return out
}

func (s *fakeStore) DueToStart(_ context.Context, now time.Time) ([]Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dueStarts++
	if len(s.dueStartErrs) > 0 {
		err := s.dueStartErrs[0]
		s.dueStartErrs = s.dueStartErrs[1:]
		return nil, err
	}
	return s.due(func(r Reservation) bool { return r.Status == StatusScheduled && !r.StartsAt.After(now) }), nil
}

func (s *fakeStore) Running(_ context.Context, now time.Time) ([]Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	return s.due(func(r Reservation) bool {
		live := r.Status == StatusActive || r.Status == StatusDegraded
		return live && !r.StartsAt.After(now) && r.EndsAt.After(now)
	}), nil
}

func (s *fakeStore) DueToEnd(_ context.Context, now time.Time) ([]Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dueEnds++
	return s.due(func(r Reservation) bool {
		live := r.Status == StatusActive || r.Status == StatusDegraded
		return live && !r.EndsAt.After(now)
	}), nil
}

func (s *fakeStore) SetReservationStatus(_ context.Context, id string, st Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.statusErrs) > 0 {
		err := s.statusErrs[0]
		s.statusErrs = s.statusErrs[1:]
		return err
	}
	s.byID[id].Status = st
	s.statusLog = append(s.statusLog, fmt.Sprintf("%s=%s", id, st))
	return nil
}

func (s *fakeStore) ReplaceNode(_ context.Context, id, oldNodeID string, now time.Time, place placement.Placer) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	marker, _ := place(placement.Request{Count: 1}, nil)
	s.replaceCalls = append(s.replaceCalls, replaceCall{ReservationID: id, OldNode: oldNodeID, Now: now, PlacerResult: marker})
	if err := s.replaceErrs[oldNodeID]; err != nil {
		return "", err
	}
	res := s.byID[id]
	newID, ok := s.replacements[oldNodeID]
	if !ok {
		capacity := &placement.CapacityError{Policy: res.Policy, Requested: 1}
		return "", fmt.Errorf("replace %s: %w", oldNodeID, capacity)
	}
	for i, nodeID := range res.NodeIDs {
		if nodeID == oldNodeID {
			res.NodeIDs[i] = newID
		}
	}
	return newID, nil
}

// StartNode checks what the real store checks, then provisions through the
// fake nodes so the event shows up in their log.
func (s *fakeStore) StartNode(_ context.Context, id, nodeID string, now time.Time) (node.Node, error) {
	s.mu.Lock()
	res := *s.byID[id]
	s.mu.Unlock()
	over := res.Status == StatusEnded || res.Status == StatusCancelled
	inWindow := !res.StartsAt.After(now) && res.EndsAt.After(now)
	if over || !inWindow || !slices.Contains(res.NodeIDs, nodeID) {
		return node.Node{}, fmt.Errorf("%w: %s cannot start %s", ErrInvalid, id, nodeID)
	}
	return s.nodes.startFor(nodeID, res)
}

func (s *fakeStore) DropNode(_ context.Context, id, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.byID[id]
	res.NodeIDs = slices.DeleteFunc(res.NodeIDs, func(n string) bool { return n == nodeID })
	s.drops = append(s.drops, nodeID)
	return nil
}

type appliedEvent struct {
	ID     string
	Event  node.Event
	Tenant string
}

type fakeNodes struct {
	mu        sync.Mutex
	nodes     map[string]node.Node
	applied   []appliedEvent
	applyErrs map[string][]error
}

func newFakeNodes() *fakeNodes {
	return &fakeNodes{nodes: map[string]node.Node{}, applyErrs: map[string][]error{}}
}

func (f *fakeNodes) set(id string, st node.State, tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[id] = node.Node{ID: id, Pod: "p1", Rack: "r1", State: st, Tenant: tenant}
}

// hold puts id in st for testTenant, held by reservation resID.
func (f *fakeNodes) hold(id string, st node.State, resID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[id] = node.Node{ID: id, Pod: "p1", Rack: "r1", State: st, Tenant: testTenant, Reservation: resID}
}

func (f *fakeNodes) get(id string) node.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[id]
}

func (f *fakeNodes) failApply(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyErrs[id] = append(f.applyErrs[id], err)
}

func (f *fakeNodes) events() []appliedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appliedEvent(nil), f.applied...)
}

func (f *fakeNodes) Get(_ context.Context, id string) (node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok {
		return node.Node{}, fmt.Errorf("node %s: %w", id, errNoSuchNode)
	}
	return n, nil
}

var errNoSuchNode = errors.New("no such node")

func (f *fakeNodes) ApplyEvent(_ context.Context, id string, ev node.Event, tenant string) (node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.applyErrs[id]; len(q) > 0 {
		f.applyErrs[id] = q[1:]
		return node.Node{}, q[0]
	}
	cur, ok := f.nodes[id]
	if !ok {
		return node.Node{}, fmt.Errorf("node %s: %w", id, errNoSuchNode)
	}
	next, err := cur.Apply(ev, tenant)
	if err != nil {
		return node.Node{}, err
	}
	f.nodes[id] = next
	f.applied = append(f.applied, appliedEvent{ID: id, Event: ev, Tenant: tenant})
	return next, nil
}

func (f *fakeNodes) startFor(id string, res Reservation) (node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.applyErrs[id]; len(q) > 0 {
		f.applyErrs[id] = q[1:]
		return node.Node{}, q[0]
	}
	cur, ok := f.nodes[id]
	if !ok {
		return node.Node{}, fmt.Errorf("node %s: %w", id, errNoSuchNode)
	}
	next, err := cur.ProvisionFor(res.Tenant, res.ID)
	if err != nil {
		return node.Node{}, err
	}
	f.nodes[id] = next
	f.applied = append(f.applied, appliedEvent{ID: id, Event: node.EventProvision, Tenant: res.Tenant})
	return next, nil
}

type fakeLeader struct {
	mu     sync.Mutex
	leader bool
	err    error
	calls  int
}

func (l *fakeLeader) TryAcquire(context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.leader, l.err
}

type fakeMetrics struct {
	mu             sync.Mutex
	degraded       int
	placementFails int
}

func (m *fakeMetrics) Degraded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.degraded++
}

func (m *fakeMetrics) PlacementFailed(placement.Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.placementFails++
}

func (m *fakeMetrics) degradedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.degraded
}

type harness struct {
	clock   *fakeClock
	store   *fakeStore
	nodes   *fakeNodes
	leader  *fakeLeader
	metrics *fakeMetrics
	logs    *bytes.Buffer
	rec     *Reconciler
}

func testConfig() Config { return Config{Tick: testTick, LockKey: testLockKey} }

func testPlacer(placement.Request, []placement.Candidate) ([]string, error) {
	return []string{placerMarker}, nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	nodes := newFakeNodes()
	h := &harness{
		clock:   &fakeClock{now: testStart},
		store:   newFakeStore(nodes),
		nodes:   nodes,
		leader:  &fakeLeader{leader: true},
		metrics: &fakeMetrics{},
		logs:    &bytes.Buffer{},
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec, err := NewReconciler(testConfig(), Deps{
		Store: h.store, Nodes: h.nodes, Leader: h.leader, Place: testPlacer,
		Clock: h.clock, Metrics: h.metrics, Log: log,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	h.rec = rec
	return h
}

// reserve adds a reservation for testTenant whose window began a minute ago
// and ends in an hour.
func (h *harness) reserve(id string, status Status, nodeIDs ...string) {
	h.store.add(Reservation{
		ID: id, Tenant: testTenant, Count: len(nodeIDs), Policy: placement.PolicySameRack,
		StartsAt: testStart.Add(-time.Minute), EndsAt: testStart.Add(time.Hour),
		Status: status, NodeIDs: nodeIDs,
	})
}

// expire moves the clock past the end of every reservation.
func (h *harness) expire() { h.clock.now = testStart.Add(2 * time.Hour) }

func (h *harness) tick(t *testing.T) {
	t.Helper()
	if err := h.rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func (h *harness) status(id string) Status { return h.store.get(id).Status }

func assertEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func assertEvents(t *testing.T, h *harness, want ...appliedEvent) {
	t.Helper()
	got := h.nodes.events()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func provisioned(id string) appliedEvent {
	return appliedEvent{ID: id, Event: node.EventProvision, Tenant: testTenant}
}

func released(id string) appliedEvent {
	return appliedEvent{ID: id, Event: node.EventRelease}
}
