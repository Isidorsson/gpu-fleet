package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/store"
)

type fakeStore struct {
	fakeReservations

	mu     sync.Mutex
	nodes  map[string]node.Node
	events map[string][]store.EventRecord
	err    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		fakeReservations: newFakeReservations(),
		nodes:            map[string]node.Node{},
		events:           map[string][]store.EventRecord{},
	}
}

func (f *fakeStore) Register(_ context.Context, n node.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[n.ID]; ok {
		return store.ErrConflict
	}
	f.nodes[n.ID] = n
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok {
		return node.Node{}, store.ErrNotFound
	}
	return n, nil
}

func (f *fakeStore) List(_ context.Context, state node.State) ([]node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []node.Node
	for _, n := range f.nodes {
		if state == "" || n.State == state {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeStore) ApplyEvent(_ context.Context, id string, ev node.Event, tenant string) (node.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.nodes[id]
	if !ok {
		return node.Node{}, store.ErrNotFound
	}
	next, err := cur.Apply(ev, tenant)
	if err != nil {
		return cur, err
	}
	f.nodes[id] = next
	f.events[id] = append(f.events[id], store.EventRecord{
		NodeID: id, From: cur.State, Event: ev, To: next.State, At: time.Now(),
	})
	return next, nil
}

func (f *fakeStore) Events(_ context.Context, id string) ([]store.EventRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[id]; !ok {
		return nil, store.ErrNotFound
	}
	return f.events[id], nil
}

func testDeps(fs *fakeStore) Deps {
	return Deps{
		Store:        fs,
		Attestations: newFakeAttestations(),
		Place:        fakePlace,
		Summarize:    (&fakeSummarizer{}).summarize,
		Now:          func() time.Time { return testNow },
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newTestServer serves the API over fs; mutate may replace any dependency.
func newTestServer(t *testing.T, fs *fakeStore, mutate ...func(*Deps)) *httptest.Server {
	t.Helper()
	deps := testDeps(fs)
	for _, m := range mutate {
		m(&deps)
	}
	srv := httptest.NewServer(NewHandler(deps))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.ContentLength != 0 {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode, out
}

const (
	testBMC = "http://10.0.0.1:8443"
	// testAKPub is the base64 of 32 bytes (0x00 to 0x1f).
	testAKPub  = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	gpu001Body = `{"id":"gpu-001","pod":"p1","rack":"r1","bmc":"` + testBMC + `","ak_pub":"` + testAKPub + `"}`
)

func TestRegisterNode(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	status, body := do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)
	if status != http.StatusCreated || body["state"] != "racked" || body["bmc"] != testBMC || body["pod"] != "p1" ||
		body["ak_pub"] != testAKPub {
		t.Fatalf("register = %d %v, want 201 racked in pod p1 with bmc %s and ak_pub %s", status, body, testBMC, testAKPub)
	}
	status, _ = do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)
	if status != http.StatusConflict {
		t.Fatalf("duplicate register = %d, want 409", status)
	}
}

func TestRegisteredBMCIsReturnedByGetAndList(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)

	_, got := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001", "")
	if got["bmc"] != testBMC {
		t.Fatalf("get = %v, want bmc %s", got, testBMC)
	}
	_, list := do(t, srv, http.MethodGet, "/v1/nodes", "")
	nodes, ok := list["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("list = %v, want one node", list)
	}
	if first, _ := nodes[0].(map[string]any); first["bmc"] != testBMC {
		t.Fatalf("listed node = %v, want bmc %s", nodes[0], testBMC)
	}
}

func TestRegisteredAKPubIsReturnedByGetAndList(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)

	_, got := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001", "")
	if got["ak_pub"] != testAKPub {
		t.Fatalf("get = %v, want ak_pub %s", got, testAKPub)
	}
	_, list := do(t, srv, http.MethodGet, "/v1/nodes", "")
	nodes, _ := list["nodes"].([]any)
	if first, _ := nodes[0].(map[string]any); len(nodes) != 1 || first["ak_pub"] != testAKPub {
		t.Fatalf("list = %v, want the node with ak_pub %s", list, testAKPub)
	}
}

func TestRegisteredPodIsReturnedByGetAndList(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)

	_, got := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001", "")
	if got["pod"] != "p1" || got["rack"] != "r1" {
		t.Fatalf("get = %v, want pod p1 rack r1", got)
	}
	_, list := do(t, srv, http.MethodGet, "/v1/nodes", "")
	nodes, _ := list["nodes"].([]any)
	if first, _ := nodes[0].(map[string]any); len(nodes) != 1 || first["pod"] != "p1" {
		t.Fatalf("list = %v, want the node in pod p1", list)
	}
}

func TestRegisterNodeValidation(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	const ak = `,"ak_pub":"` + testAKPub + `"`
	tests := []struct {
		body string
		want int
	}{
		{`not json`, http.StatusBadRequest},
		{`{"id":"gpu-001","pod":"p1","rack":"r1","bmc":"http://h"` + ak + `,"extra":1}`, http.StatusBadRequest},
		{`{"id":"","pod":"p1","rack":"r1","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
		{`{"id":"has space","pod":"p1","rack":"r1","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
		{`{"id":"gpu-001","pod":"p1","rack":"","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
		{`{"id":"gpu-001","pod":"p1","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
		{`{"id":"gpu-001","pod":"","rack":"r1","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
		{`{"id":"gpu-001","rack":"r1","bmc":"http://h"` + ak + `}`, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		if status, body := do(t, srv, http.MethodPost, "/v1/nodes", tt.body); status != tt.want {
			t.Errorf("POST %s = %d %v, want %d", tt.body, status, body, tt.want)
		}
	}
}

func TestRegisterNodeAKPubValidation(t *testing.T) {
	key := func(n int) []byte { return bytes.Repeat([]byte{0xfb}, n) }
	for _, tt := range []struct {
		name string
		ak   string
		want int
	}{
		{"valid", base64.StdEncoding.EncodeToString(key(32)), http.StatusCreated},
		{"empty", "", http.StatusUnprocessableEntity},
		{"not base64", "!!!not base64!!!", http.StatusUnprocessableEntity},
		{"url alphabet", base64.URLEncoding.EncodeToString(key(32)), http.StatusUnprocessableEntity},
		{"unpadded", base64.RawStdEncoding.EncodeToString(key(32)), http.StatusUnprocessableEntity},
		{"too short", base64.StdEncoding.EncodeToString(key(31)), http.StatusUnprocessableEntity},
		{"too long", base64.StdEncoding.EncodeToString(key(33)), http.StatusUnprocessableEntity},
		{"private key sized", base64.StdEncoding.EncodeToString(key(64)), http.StatusUnprocessableEntity},
		{"hex", hex.EncodeToString(key(32)), http.StatusUnprocessableEntity},
		{"leading space", " " + base64.StdEncoding.EncodeToString(key(32)), http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			srv := newTestServer(t, fs)
			body, err := json.Marshal(map[string]string{"id": "gpu-001", "pod": "p1", "rack": "r1", "bmc": testBMC, "ak_pub": tt.ak})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			status, resp := do(t, srv, http.MethodPost, "/v1/nodes", string(body))
			if status != tt.want {
				t.Errorf("ak_pub %q = %d %v, want %d", tt.ak, status, resp, tt.want)
			}
			if _, registered := fs.nodes["gpu-001"]; registered != (tt.want == http.StatusCreated) {
				t.Errorf("node registered = %v after %d", registered, status)
			}
		})
	}
}

func TestRegisterNodeRequiresAKPub(t *testing.T) {
	fs := newFakeStore()
	srv := newTestServer(t, fs)
	status, body := do(t, srv, http.MethodPost, "/v1/nodes", `{"id":"gpu-001","pod":"p1","rack":"r1","bmc":"`+testBMC+`"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("register without ak_pub = %d %v, want 422", status, body)
	}
	if len(fs.nodes) != 0 {
		t.Errorf("node registered without an attestation key: %v", fs.nodes)
	}
}

func TestRegisterNodeBMCValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		bmc  string
		want int
	}{
		{"http", "http://10.0.0.1:8443", http.StatusCreated},
		{"https with path", "https://bmc.example.com/redfish-proxy", http.StatusCreated},
		{"ipv6 host", "http://[::1]:8443", http.StatusCreated},
		{"empty", "", http.StatusUnprocessableEntity},
		{"relative", "/redfish/v1", http.StatusUnprocessableEntity},
		{"no scheme", "10.0.0.1:8443", http.StatusUnprocessableEntity},
		{"host and port as scheme", "localhost:8443", http.StatusUnprocessableEntity},
		{"ftp", "ftp://10.0.0.1", http.StatusUnprocessableEntity},
		{"no host", "http://", http.StatusUnprocessableEntity},
		{"path only after scheme", "http:///redfish", http.StatusUnprocessableEntity},
		{"unparseable", "http://[::1", http.StatusUnprocessableEntity},
		{"query", "http://10.0.0.1?x=1", http.StatusUnprocessableEntity},
		{"empty query", "http://10.0.0.1/?", http.StatusUnprocessableEntity},
		{"fragment", "http://10.0.0.1/#top", http.StatusUnprocessableEntity},
		{"empty fragment", "http://10.0.0.1/#", http.StatusUnprocessableEntity},
		{"credentials", "http://admin:secret@10.0.0.1", http.StatusUnprocessableEntity},
		{"leading space", " http://10.0.0.1", http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, newFakeStore())
			body, err := json.Marshal(map[string]string{"id": "gpu-001", "pod": "p1", "rack": "r1", "bmc": tt.bmc, "ak_pub": testAKPub})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if status, resp := do(t, srv, http.MethodPost, "/v1/nodes", string(body)); status != tt.want {
				t.Errorf("bmc %q = %d %v, want %d", tt.bmc, status, resp, tt.want)
			}
		})
	}
}

func TestRegisterNodeRequiresBMC(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	status, body := do(t, srv, http.MethodPost, "/v1/nodes", `{"id":"gpu-001","pod":"p1","rack":"r1","ak_pub":"`+testAKPub+`"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("register without bmc = %d %v, want 422", status, body)
	}
}

func TestLifecycleOverHTTP(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	do(t, srv, http.MethodPost, "/v1/nodes", gpu001Body)

	steps := []struct {
		body       string
		wantStatus int
		wantState  string
	}{
		{`{"event":"start_inspection"}`, http.StatusOK, "inspecting"},
		{`{"event":"inspection_passed"}`, http.StatusOK, "available"},
		{`{"event":"provision"}`, http.StatusUnprocessableEntity, ""},
		{`{"event":"provision","tenant":"acme"}`, http.StatusOK, "provisioning"},
		{`{"event":"provision","tenant":"other"}`, http.StatusConflict, ""},
		{`{"event":"explode"}`, http.StatusUnprocessableEntity, ""},
	}
	for _, s := range steps {
		status, body := do(t, srv, http.MethodPost, "/v1/nodes/gpu-001/events", s.body)
		if status != s.wantStatus {
			t.Fatalf("event %s = %d %v, want %d", s.body, status, body, s.wantStatus)
		}
		if s.wantState != "" && body["state"] != s.wantState {
			t.Fatalf("event %s state = %v, want %s", s.body, body["state"], s.wantState)
		}
	}

	_, got := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001", "")
	if got["tenant"] != "acme" {
		t.Fatalf("node = %v, want tenant acme", got)
	}
	_, history := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/events", "")
	if events, ok := history["events"].([]any); !ok || len(events) != 3 {
		t.Fatalf("history = %v, want 3 events", history)
	}
}

func TestUnknownNodeIs404(t *testing.T) {
	srv := newTestServer(t, newFakeStore())
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/nodes/nope", ""},
		{http.MethodGet, "/v1/nodes/nope/events", ""},
		{http.MethodPost, "/v1/nodes/nope/events", `{"event":"start_inspection"}`},
	} {
		if status, _ := do(t, srv, req.method, req.path, req.body); status != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.method, req.path, status)
		}
	}
}

func TestListNodesFiltersByState(t *testing.T) {
	fs := newFakeStore()
	fs.nodes["a"] = node.Node{ID: "a", Pod: "p1", Rack: "r1", State: node.StateAvailable}
	fs.nodes["b"] = node.Node{ID: "b", Pod: "p1", Rack: "r1", State: node.StateRacked}
	srv := newTestServer(t, fs)

	status, body := do(t, srv, http.MethodGet, "/v1/nodes?state=available", "")
	nodes, ok := body["nodes"].([]any)
	if status != http.StatusOK || !ok || len(nodes) != 1 {
		t.Fatalf("list = %d %v, want one available node", status, body)
	}
	if status, _ := do(t, srv, http.MethodGet, "/v1/nodes?state=on_fire", ""); status != http.StatusBadRequest {
		t.Fatalf("bad state filter = %d, want 400", status)
	}
}

func TestStoreFailureHidesDetails(t *testing.T) {
	fs := newFakeStore()
	fs.err = fmt.Errorf("connection reset: password=hunter2")
	srv := newTestServer(t, fs)
	status, body := do(t, srv, http.MethodGet, "/v1/nodes", "")
	if status != http.StatusInternalServerError || body["error"] != "internal error" {
		t.Fatalf("store failure = %d %v, want generic 500", status, body)
	}
}
