package attestsvc

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/node"
)

const (
	testNodeID  = "node-001"
	testBMCURL  = "http://bmc.test:8443"
	testVersion = "2026-10-19.1"
)

// testNow has a sub-second part and a non-UTC zone so truncation and
// normalization are visible in results.
var testNow = time.Date(2030, 6, 1, 14, 0, 0, 789_000_000, time.FixedZone("CEST", 2*3600))

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type issued struct {
	nodeID, nonce string
	expires       time.Time
}

type consumed struct {
	nodeID, nonce string
	now           time.Time
}

// fakeStore records calls, in order, into the shared log.
type fakeStore struct {
	log *callLog

	mu       sync.Mutex
	issued   []issued
	consumed []consumed
	recorded []attest.Result

	issueErr, consumeErr, recordErr error
	latest                          attest.Result
	latestErr                       error
}

func (f *fakeStore) IssueNonce(_ context.Context, nodeID, nonce string, expires time.Time) error {
	f.log.add("issue")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issueErr != nil {
		return f.issueErr
	}
	f.issued = append(f.issued, issued{nodeID, nonce, expires})
	return nil
}

func (f *fakeStore) ConsumeNonce(_ context.Context, nodeID, nonce string, now time.Time) error {
	f.log.add("consume")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumeErr != nil {
		return f.consumeErr
	}
	f.consumed = append(f.consumed, consumed{nodeID, nonce, now})
	return nil
}

func (f *fakeStore) RecordAttestation(_ context.Context, r attest.Result) error {
	f.log.add("record")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, r)
	return nil
}

func (f *fakeStore) LatestAttestation(_ context.Context, _ string) (attest.Result, error) {
	return f.latest, f.latestErr
}

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type evidenceCall struct{ id, nonce string }

type fakeAgent struct {
	log      *callLog
	evidence attest.Evidence
	err      error
	calls    []evidenceCall
}

func (a *fakeAgent) Evidence(_ context.Context, id, nonce string) (attest.Evidence, error) {
	a.log.add("fetch")
	a.calls = append(a.calls, evidenceCall{id, nonce})
	return a.evidence, a.err
}

type verifyCall struct {
	evidence attest.Evidence
	expected attest.Expected
	ref      attest.Reference
}

type harness struct {
	svc     *Service
	store   *fakeStore
	agent   *fakeAgent
	clock   *fakeClock
	log     *callLog
	key     ed25519.PrivateKey
	ref     attest.Reference
	dialed  []string
	verify  []verifyCall
	verdict attest.Result // returned by the fake Verify
}

// newHarness builds a Service over fakes. The fake Verify returns h.verdict
// with the call's node, nonce and reference version filled in, as the real one
// does, unless the test overrides those fields.
func newHarness(t *testing.T) *harness {
	t.Helper()
	log := &callLog{}
	h := &harness{
		store: &fakeStore{log: log},
		agent: &fakeAgent{log: log, evidence: attest.Evidence{NodeID: testNodeID, QuoteSig: "c2ln"}},
		clock: &fakeClock{now: testNow},
		log:   log,
		ref:   attest.Reference{Version: testVersion},
		verdict: attest.Result{
			Verdict:            attest.VerdictPass,
			MeasurementsDigest: "00ff",
		},
	}
	_, h.key = mustKey(t, 1)
	svc, err := New(Config{SigningKey: h.key, Reference: h.ref}, Deps{
		Store: h.store,
		Dial: func(bmcURL string) Agent {
			h.dialed = append(h.dialed, bmcURL)
			return h.agent
		},
		Verify: func(ev attest.Evidence, exp attest.Expected, ref attest.Reference) attest.Result {
			log.add("verify")
			h.verify = append(h.verify, verifyCall{ev, exp, ref})
			res := h.verdict
			res.NodeID, res.Nonce, res.ReferenceVersion = exp.NodeID, exp.Nonce, ref.Version
			return res
		},
		Clock: h.clock,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

func mustKey(t *testing.T, seedByte byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("public key is not Ed25519")
	}
	return pub, priv
}

// enrolledNode is a node whose AK is the public half of key seed 2.
func enrolledNode(t *testing.T) node.Node {
	t.Helper()
	pub, _ := mustKey(t, 2)
	return node.Node{
		ID:    testNodeID,
		BMC:   testBMCURL,
		AKPub: base64.StdEncoding.EncodeToString(pub),
		State: node.StateInspecting,
	}
}
