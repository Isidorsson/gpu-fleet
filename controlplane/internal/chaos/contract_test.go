package chaos

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gpu-fleet/controlplane/internal/bmcsim"
)

// These tests run the chaos clients against the real bmcsim handler, so a
// drift in the wire format shows up here and not in the demo.

func newRealBMCSim(t *testing.T, nodes int, errorRate float64) *httptest.Server {
	t.Helper()
	h, err := bmcsim.NewHandler(bmcsim.Config{
		Nodes: nodes, Seed: 1, BMCErrorRate: errorRate, User: testBMCUser, Password: testBMCPassword,
	}, bmcsim.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestStartupAgainstTheRealBMCSimRegistersValidAttestationKeys(t *testing.T) {
	sim := newRealBMCSim(t, 50, 0.2)

	var mu sync.Mutex
	registered := map[string]NodeSpec{}
	rec := &recorder{}
	cp := newFake(t, "cp", rec, func(w http.ResponseWriter, c call) {
		if c.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var spec NodeSpec
		if err := json.Unmarshal([]byte(c.Body), &spec); err != nil {
			t.Errorf("bad register body %q: %v", c.Body, err)
		}
		mu.Lock()
		registered[spec.ID] = spec
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})

	cfg := Config{
		ControlPlaneURL: cp.URL, BMCSimURL: sim.URL, NodeBMCURL: testNodeBMCURL,
		BMCUser: testBMCUser, BMCPassword: testBMCPassword,
	}
	s := New(cfg, discardLog())
	s.backoff = fastBackoff
	if err := s.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(registered) != 50 {
		t.Fatalf("registered %d nodes, want 50", len(registered))
	}
	for id, spec := range registered {
		key, err := base64.StdEncoding.DecodeString(spec.AKPub)
		if err != nil || len(key) != ed25519.PublicKeySize {
			t.Errorf("%s: ak_pub %q is not a base64 Ed25519 key (the control plane would answer 422)", id, spec.AKPub)
		}
	}
}

func TestEveryImplantComponentIsAcceptedByTheRealBMCSim(t *testing.T) {
	sim := newRealBMCSim(t, 3, 0)
	bmc := NewBMC(sim.URL, testBMCUser, testBMCPassword)
	ctx := context.Background()
	for _, component := range implantComponents {
		if err := bmc.Tamper(ctx, "node-001", component); err != nil {
			t.Errorf("tamper %q: %v", component, err)
		}
	}
	if err := bmc.ClearTamper(ctx, "node-001"); err != nil {
		t.Errorf("clear tamper: %v", err)
	}
	if err := bmc.ClearTamper(ctx, "node-001"); err != nil {
		t.Errorf("clearing twice must be fine: %v", err)
	}
	if err := bmc.Tamper(ctx, "node-999", "kernel"); !hasStatus(err, http.StatusNotFound) {
		t.Errorf("an unknown system must be a 404, got %v", err)
	}
}

func TestRealBMCSimRejectsWrongCredentials(t *testing.T) {
	sim := newRealBMCSim(t, 3, 0)
	bmc := NewBMC(sim.URL, testBMCUser, "wrong")
	if _, err := bmc.Systems(context.Background()); !hasStatus(err, http.StatusUnauthorized) {
		t.Errorf("want 401, got %v", err)
	}
}
