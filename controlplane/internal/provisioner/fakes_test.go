package provisioner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

const staleOwner = "another-worker"

// fakeClock advances instantly: Sleep adds to Now and never blocks.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(count int, d time.Duration)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
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
		hook(count, d)
	}
	return ctx.Err()
}

func (c *fakeClock) sleepsOf(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sleeps {
		if s == d {
			n++
		}
	}
	return n
}

// sleepsExcept returns the sleeps that are not poll or idle waits, which are
// the retry backoffs.
func (c *fakeClock) sleepsExcept(skip ...time.Duration) []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
next:
	for _, s := range c.sleeps {
		for _, k := range skip {
			if s == k {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

type appliedEvent struct {
	ID    string
	Event node.Event
	From  node.State
	To    node.State
}

// fakeStore implements lease semantics in memory. Leases never expire; tests
// take them away with loseLease. Hooks run without the lock held.
type fakeStore struct {
	mu       sync.Mutex
	nodes    map[string]node.Node
	order    []string
	leases   map[string]string
	applied  []appliedEvent
	releases []string
	claims   int
	renews   int

	onRenew  func(call int)
	claimErr error
	applyErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{nodes: map[string]node.Node{}, leases: map[string]string{}}
}

func (s *fakeStore) add(n node.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	s.order = append(s.order, n.ID)
}

func (s *fakeStore) get(id string) node.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodes[id]
}

func (s *fakeStore) holder(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leases[id]
}

// operatorApply changes a node the way the API does: no lease check.
func (s *fakeStore) operatorApply(id string, ev node.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.nodes[id].Apply(ev, "")
	if err != nil {
		panic(fmt.Sprintf("operatorApply %s: %v", ev, err))
	}
	s.nodes[id] = next
}

func (s *fakeStore) loseLease(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[id] = staleOwner
}

func (s *fakeStore) appliedEvents() []node.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]node.Event, 0, len(s.applied))
	for _, a := range s.applied {
		out = append(out, a.Event)
	}
	return out
}

func (s *fakeStore) ClaimWork(_ context.Context, owner string, _ time.Duration) (node.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		err := s.claimErr
		s.claimErr = nil
		return node.Node{}, err
	}
	for _, id := range s.order {
		n := s.nodes[id]
		if s.leases[id] == "" && claimable(n.State) {
			s.leases[id] = owner
			return n, nil
		}
	}
	return node.Node{}, store.ErrNoWork
}

func claimable(st node.State) bool {
	switch st {
	case node.StateRacked, node.StateInspecting, node.StateProvisioning, node.StateWiping:
		return true
	}
	return false
}

func (s *fakeStore) RenewLease(_ context.Context, id, owner string, _ time.Duration) error {
	s.mu.Lock()
	s.renews++
	call, hook := s.renews, s.onRenew
	s.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[id] != owner {
		return store.ErrLeaseLost
	}
	return nil
}

// ReleaseLease fails on a canceled context, so a passing release proves the
// worker used a fresh one.
func (s *fakeStore) ReleaseLease(ctx context.Context, id, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, id)
	if s.leases[id] == owner {
		delete(s.leases, id)
	}
	return nil
}

func (s *fakeStore) ApplyEventAsOwner(_ context.Context, id, owner string, ev node.Event, tenant string) (node.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applyErr != nil {
		err := s.applyErr
		s.applyErr = nil
		return node.Node{}, err
	}
	if s.leases[id] != owner {
		return node.Node{}, store.ErrLeaseLost
	}
	cur := s.nodes[id]
	next, err := cur.Apply(ev, tenant)
	if err != nil {
		return node.Node{}, err
	}
	s.nodes[id] = next
	s.applied = append(s.applied, appliedEvent{ID: id, Event: ev, From: cur.State, To: next.State})
	return next, nil
}

// fakeMachine is both the BMC and the deploy agent of every dialed node. Every
// call is recorded as a string such as "set_task:inspect" or "reset:ForceOff";
// errors are queued per call string and consumed one per call.
type fakeMachine struct {
	mu      sync.Mutex
	calls   []string
	errs    map[string][]error
	scripts map[deployagent.Task]*phaseScript
	current deployagent.Task
	dialed  []string

	// statusFn, when set, replaces the scripted status.
	statusFn func() deployagent.Status
}

// phaseScript replays phases in order and then repeats the last one.
type phaseScript struct {
	phases []deployagent.Phase
	next   int
}

func (p *phaseScript) advance() deployagent.Phase {
	i := min(p.next, len(p.phases)-1)
	p.next++
	return p.phases[i]
}

func newFakeMachine() *fakeMachine {
	return &fakeMachine{errs: map[string][]error{}, scripts: map[deployagent.Task]*phaseScript{}}
}

func (m *fakeMachine) script(t deployagent.Task, phases ...deployagent.Phase) {
	m.scripts[t] = &phaseScript{phases: phases}
}

func (m *fakeMachine) failNext(call string, errs ...error) {
	m.errs[call] = append(m.errs[call], errs...)
}

func (m *fakeMachine) dial(url string) (BMC, Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialed = append(m.dialed, url)
	return m, m
}

// do records call and returns its next queued error, if any.
func (m *fakeMachine) do(call string) error {
	m.calls = append(m.calls, call)
	q := m.errs[call]
	if len(q) == 0 {
		return nil
	}
	m.errs[call] = q[1:]
	return q[0]
}

func (m *fakeMachine) SetTask(_ context.Context, _ string, t deployagent.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.do("set_task:" + string(t)); err != nil {
		return err
	}
	m.current = t
	if sc := m.scripts[t]; sc != nil {
		sc.next = 0
	}
	return nil
}

func (m *fakeMachine) SetPXEBootOnce(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.do("pxe_once")
}

func (m *fakeMachine) Reset(_ context.Context, _ string, t redfish.ResetType) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.do("reset:" + string(t))
}

func (m *fakeMachine) Status(context.Context, string) (deployagent.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.do("status"); err != nil {
		return deployagent.Status{}, err
	}
	if m.statusFn != nil {
		return m.statusFn(), nil
	}
	phase := deployagent.PhaseSucceeded
	if sc := m.scripts[m.current]; sc != nil {
		phase = sc.advance()
	}
	return deployagent.Status{Task: m.current, Phase: phase}, nil
}

func (m *fakeMachine) callLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *fakeMachine) count(call string) int {
	n := 0
	for _, c := range m.callLog() {
		if c == call {
			n++
		}
	}
	return n
}

// controlCalls is the call log without status polls.
func (m *fakeMachine) controlCalls() []string {
	var out []string
	for _, c := range m.callLog() {
		if c != "status" {
			out = append(out, c)
		}
	}
	return out
}
