package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNodeBMCURL = "http://bmcsim:8443"

// fleetFake stands in for bmcsim and the control plane during startup. The
// failure counters are how many requests of that kind answer 503 first.
type fleetFake struct {
	cp  *httptest.Server
	bmc *httptest.Server
	rec *recorder

	mu               sync.Mutex
	posted           []NodeSpec
	existing         map[string]bool
	healthFailures   int
	systemsFailures  int
	identityFailures int
	postFailures     int
	systemsCalls     int
	healthCalls      int
}

func newFleetFake(t *testing.T, nodes int) *fleetFake {
	t.Helper()
	f := &fleetFake{existing: map[string]bool{}, rec: &recorder{}}
	f.cp = newFake(t, "cp", f.rec, f.controlPlane)
	f.bmc = newFake(t, "bmc", f.rec, func(w http.ResponseWriter, c call) { f.bmcsim(w, c, nodes) })
	return f
}

func (f *fleetFake) callsTo(server string) int {
	n := 0
	for _, c := range f.rec.all() {
		if c.Server == server {
			n++
		}
	}
	return n
}

func (f *fleetFake) fail(counter *int) bool {
	if *counter > 0 {
		*counter--
		return true
	}
	return false
}

func (f *fleetFake) controlPlane(w http.ResponseWriter, c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case c.Path == "/healthz":
		f.healthCalls++
		if f.fail(&f.healthFailures) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case c.Method == http.MethodPost && c.Path == "/v1/nodes":
		f.registerNode(w, c)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fleetFake) registerNode(w http.ResponseWriter, c call) {
	if f.fail(&f.postFailures) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var spec NodeSpec
	if err := json.Unmarshal([]byte(c.Body), &spec); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.posted = append(f.posted, spec)
	if f.existing[spec.ID] {
		writeJSONBody(w, http.StatusConflict, map[string]string{"error": "node already exists"})
		return
	}
	writeJSONBody(w, http.StatusCreated, spec)
}

func (f *fleetFake) bmcsim(w http.ResponseWriter, c call, nodes int) {
	if !requireBMCAuth(w, c) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case c.Path == "/redfish/v1/Systems":
		f.systemsCalls++
		if f.fail(&f.systemsFailures) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		members := make([]map[string]string, 0, nodes)
		for i := 1; i <= nodes; i++ {
			members = append(members, map[string]string{"@odata.id": fmt.Sprintf("/redfish/v1/Systems/node-%03d", i)})
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"Members": members, "Members@odata.count": nodes})
	case strings.HasPrefix(c.Path, "/agent/") && strings.HasSuffix(c.Path, "/identity"):
		if f.fail(&f.identityFailures) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(c.Path, "/agent/"), "/identity")
		writeJSONBody(w, http.StatusOK, map[string]string{"ak_pub": "ak-" + id})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fleetFake) config() Config {
	return Config{
		ControlPlaneURL: f.cp.URL,
		BMCSimURL:       f.bmc.URL,
		NodeBMCURL:      testNodeBMCURL,
		BMCUser:         testBMCUser,
		BMCPassword:     testBMCPassword,
	}
}

func (f *fleetFake) service() *Service {
	s := New(f.config(), discardLog())
	s.backoff = fastBackoff
	return s
}

// specs returns the last body posted for each node and how many posts there were.
func (f *fleetFake) specs() (map[string]NodeSpec, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]NodeSpec{}
	for _, spec := range f.posted {
		out[spec.ID] = spec
	}
	return out, len(f.posted)
}

func (f *fleetFake) readCounts() (health, systems int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthCalls, f.systemsCalls
}

func TestStartupRegistersFiftyNodesLikeSeedSh(t *testing.T) {
	f := newFleetFake(t, 50)
	if err := f.service().Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	specs, posts := f.specs()
	if len(specs) != 50 || posts != 50 {
		t.Fatalf("registered %d distinct nodes in %d requests, want 50 in 50", len(specs), posts)
	}

	pods, racks := map[string]int{}, map[string]int{}
	for i := 1; i <= 50; i++ {
		id := fmt.Sprintf("node-%03d", i)
		spec, ok := specs[id]
		if !ok {
			t.Fatalf("%s was not registered", id)
		}
		wantRack := fmt.Sprintf("r%d", (i-1)/8+1)
		wantPod := "p1"
		if (i-1)/8+1 > 4 {
			wantPod = "p2"
		}
		if spec.Rack != wantRack || spec.Pod != wantPod {
			t.Errorf("%s placed in %s/%s, want %s/%s", id, spec.Pod, spec.Rack, wantPod, wantRack)
		}
		if spec.BMC != testNodeBMCURL {
			t.Errorf("%s bmc = %q, want CHAOS_NODE_BMC_URL", id, spec.BMC)
		}
		if spec.AKPub != "ak-"+id {
			t.Errorf("%s ak_pub = %q, want the key from its identity endpoint", id, spec.AKPub)
		}
		pods[spec.Pod]++
		racks[spec.Rack]++
	}
	if pods["p1"] != 32 || pods["p2"] != 18 {
		t.Errorf("pods = %v, want p1=32 p2=18", pods)
	}
	for _, rack := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		if racks[rack] != 8 {
			t.Errorf("rack %s has %d nodes, want 8", rack, racks[rack])
		}
	}
	if racks["r7"] != 2 || len(racks) != 7 {
		t.Errorf("racks = %v, want r1..r6 with 8 and r7 with 2", racks)
	}
}

func TestStartupToleratesAlreadyRegisteredNodes(t *testing.T) {
	f := newFleetFake(t, 50)
	for i := 1; i <= 50; i += 3 {
		f.existing[fmt.Sprintf("node-%03d", i)] = true
	}
	s := f.service()
	if err := s.Startup(context.Background()); err != nil {
		t.Fatalf("409 must mean already registered, got %v", err)
	}
	if err := s.Startup(context.Background()); err != nil {
		t.Fatalf("a second startup must be idempotent too, got %v", err)
	}
	specs, posts := f.specs()
	if len(specs) != 50 || posts != 100 {
		t.Errorf("registered %d distinct nodes in %d requests, want 50 in 100", len(specs), posts)
	}
}

func TestStartupWaitsThroughInitialFailures(t *testing.T) {
	f := newFleetFake(t, 12)
	f.healthFailures, f.systemsFailures, f.identityFailures, f.postFailures = 3, 2, 2, 2
	if err := f.service().Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if specs, _ := f.specs(); len(specs) != 12 {
		t.Errorf("registered %d nodes, want 12", len(specs))
	}
	if health, systems := f.readCounts(); health != 4 || systems != 3 {
		t.Errorf("healthz called %d times and systems %d times, want 4 and 3", health, systems)
	}
}

func TestStartupFailsFastOnBadCredentials(t *testing.T) {
	f := newFleetFake(t, 5)
	cfg := f.config()
	cfg.BMCPassword = "wrong"
	s := New(cfg, discardLog())
	s.backoff = fastBackoff

	err := s.Startup(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("want a 401, got %v", err)
	}
	if n := f.callsTo("bmc"); n != 1 {
		t.Errorf("bmcsim was asked %d times; a 401 is permanent and must not be retried", n)
	}
	if _, posts := f.specs(); posts != 0 {
		t.Errorf("nothing may be registered, got %d posts", posts)
	}
}

func TestStartupFailsFastOnARejectedRegistration(t *testing.T) {
	f := newFleetFake(t, 5)
	cfg := f.config()
	cfg.NodeBMCURL = "not-a-url"
	rejecting := newFake(t, "cp", f.rec, func(w http.ResponseWriter, c call) {
		if c.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid request"})
	})
	cfg.ControlPlaneURL = rejecting.URL
	s := New(cfg, discardLog())
	s.backoff = fastBackoff

	err := s.Startup(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity {
		t.Fatalf("want a 422, got %v", err)
	}
	posts := 0
	for _, c := range f.rec.all() {
		if c.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d registration attempts; a 422 is permanent", posts)
	}
}

func TestStartupGivesUpWhenADependencyStaysDown(t *testing.T) {
	f := newFleetFake(t, 5)
	f.healthFailures = 1 << 30
	s := f.service()
	s.backoff = Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond, Timeout: 60 * time.Millisecond}

	err := s.Startup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("want a bounded wait that gives up, got %v", err)
	}
	if health, _ := f.readCounts(); health < 3 {
		t.Errorf("only %d attempts; it should have retried", health)
	}
}

func TestStartupStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFleetFake(t, 5)
	f.healthFailures = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)

	err := f.service().Startup(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestStartupRejectsAnEmptyFleet(t *testing.T) {
	f := newFleetFake(t, 0)
	if err := f.service().Startup(context.Background()); err == nil {
		t.Error("an empty systems list is a misconfiguration")
	}
}

func TestTopology(t *testing.T) {
	tests := []struct {
		id, pod, rack string
	}{
		{"node-001", "p1", "r1"},
		{"node-008", "p1", "r1"},
		{"node-009", "p1", "r2"},
		{"node-032", "p1", "r4"},
		{"node-033", "p2", "r5"},
		{"node-040", "p2", "r5"},
		{"node-041", "p2", "r6"},
		{"node-049", "p2", "r7"},
		{"node-050", "p2", "r7"},
		{"gpu-host-17", "p1", "r3"},
		{"node-1000", "p2", "r125"},
	}
	for _, tt := range tests {
		pod, rack, err := Topology(tt.id)
		if err != nil || pod != tt.pod || rack != tt.rack {
			t.Errorf("Topology(%q) = %s, %s, %v; want %s, %s", tt.id, pod, rack, err, tt.pod, tt.rack)
		}
	}
	for _, id := range []string{"node", "node-", "node-abc", "node-000", "rack-a"} {
		if _, _, err := Topology(id); err == nil {
			t.Errorf("Topology(%q) should fail", id)
		}
	}
}

func TestConnectionRefusedIsTransientAndABadStatusIsNot(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	cp := NewControlPlane(srv.URL)
	if err := cp.Healthz(context.Background()); transient(err) || err == nil {
		t.Fatalf("a 404 must be a permanent error, got %v (transient %v)", err, transient(err))
	}
	srv.Close()
	if err := cp.Healthz(context.Background()); !transient(err) {
		t.Errorf("a refused connection must be worth retrying, got %v", err)
	}
}
