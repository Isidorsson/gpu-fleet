package chaos

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

const (
	testBMCUser     = "sim"
	testBMCPassword = "sim-dev-only"
)

func newRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, rngStream)) }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func nodeOf(id string, state node.State) NodeInfo { return NodeInfo{ID: id, State: state} }

func resOf(id string, status ReservationStatus, nodes ...string) ReservationInfo {
	return ReservationInfo{ID: id, Tenant: "t-" + id, Status: status, Nodes: nodes}
}

func agentSet(ids ...string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return set
}

func snapOf(nodes []NodeInfo, reservations []ReservationInfo, agentIDs ...string) Snapshot {
	return Snapshot{Nodes: nodes, Reservations: reservations, AgentBacked: agentSet(agentIDs...)}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v: %q", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v: %q", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON differs\n got: %s\nwant: %s", got, want)
	}
}

// call is one HTTP request a fake server saw.
type call struct {
	Server string
	Method string
	Path   string
	Body   string
	User   string
	Pass   string
	Auth   bool
}

func (c call) String() string { return c.Server + " " + c.Method + " " + c.Path }

// recorder collects calls from several fake servers in arrival order.
type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) add(c call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) all() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.calls...)
}

func (r *recorder) summary() []string {
	var out []string
	for _, c := range r.all() {
		out = append(out, c.String())
	}
	return out
}

func (r *recorder) only(t *testing.T) call {
	t.Helper()
	calls := r.all()
	if len(calls) != 1 {
		t.Fatalf("want exactly 1 call, got %v", r.summary())
	}
	return calls[0]
}

type respondFunc func(w http.ResponseWriter, c call)

// newFake starts a server that records every request, then lets respond
// answer; a nil respond answers 204.
func newFake(t *testing.T, name string, rec *recorder, respond respondFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("%s: read body: %v", name, err)
		}
		user, pass, ok := r.BasicAuth()
		c := call{Server: name, Method: r.Method, Path: r.URL.Path, Body: string(body), User: user, Pass: pass, Auth: ok}
		rec.add(c)
		if respond == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		respond(w, c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// requireBMCAuth answers 401 unless the call carries the test credentials,
// like bmcsim does.
func requireBMCAuth(w http.ResponseWriter, c call) bool {
	if c.Auth && c.User == testBMCUser && c.Pass == testBMCPassword {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

var fastBackoff = Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond, Timeout: 10 * time.Second}
