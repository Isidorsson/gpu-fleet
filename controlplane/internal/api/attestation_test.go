package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/store"
)

type fakeAttestations struct {
	pub     ed25519.PublicKey
	results map[string]attest.Result
	err     error
}

func newFakeAttestations() *fakeAttestations {
	return &fakeAttestations{pub: ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)), results: map[string]attest.Result{}}
}

func (f *fakeAttestations) Latest(_ context.Context, nodeID string) (attest.Result, error) {
	if f.err != nil {
		return attest.Result{}, f.err
	}
	r, ok := f.results[nodeID]
	if !ok {
		return attest.Result{}, fmt.Errorf("%w: %s", store.ErrAttestationNotFound, nodeID)
	}
	return r, nil
}

func (f *fakeAttestations) PublicKey() ed25519.PublicKey { return f.pub }

// signedAttestations holds results signed by a real key, so tests can verify
// what the API returns the way a tenant would.
func signedAttestations(t *testing.T, results ...attest.Result) *fakeAttestations {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeAttestations{pub: pub, results: map[string]attest.Result{}}
	for _, r := range results {
		r.Signature = ed25519.Sign(priv, attest.ResultMessage(r))
		f.results[r.NodeID] = r
	}
	return f
}

func withAttestations(a Attestations) func(*Deps) {
	return func(d *Deps) { d.Attestations = a }
}

var attestedAt = time.Date(2030, 6, 1, 12, 0, 5, 0, time.UTC)

func failedResult() attest.Result {
	return attest.Result{
		NodeID:             "gpu-001",
		Verdict:            attest.VerdictFail,
		Reasons:            []string{"bootloader digest not in reference values", "gpu 3 vbios not allowed"},
		Nonce:              "0a1b",
		MeasurementsDigest: "cafe",
		ReferenceVersion:   "2026-10-19.1",
		AttestedAt:         attestedAt,
	}
}

func TestGetAttestationReturnsTheSignedResult(t *testing.T) {
	res := failedResult()
	fa := signedAttestations(t, res)
	srv := newTestServer(t, newFakeStore(), withAttestations(fa))

	status, body := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/attestation", "")

	if status != http.StatusOK {
		t.Fatalf("status = %d %v, want 200", status, body)
	}
	want := map[string]any{
		"node_id":             "gpu-001",
		"verdict":             "fail",
		"reasons":             []any{"bootloader digest not in reference values", "gpu 3 vbios not allowed"},
		"nonce":               "0a1b",
		"measurements_digest": "cafe",
		"reference_version":   "2026-10-19.1",
		"attested_at":         "2030-06-01T12:00:05Z",
		"signature":           base64.StdEncoding.EncodeToString(fa.results["gpu-001"].Signature),
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

func TestAttestationResponseVerifiesOffline(t *testing.T) {
	fa := signedAttestations(t, failedResult())
	srv := newTestServer(t, newFakeStore(), withAttestations(fa))
	_, att := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/attestation", "")
	_, keys := do(t, srv, http.MethodGet, "/v1/attestation/keys", "")

	list, _ := keys["keys"].([]any)
	first, _ := list[0].(map[string]any)
	pub := mustB64(t, first["public_key"])
	sig := mustB64(t, att["signature"])
	signedAt, err := time.Parse(time.RFC3339, str(att["attested_at"]))
	if err != nil {
		t.Fatalf("attested_at: %v", err)
	}
	var reasons []string
	for _, r := range att["reasons"].([]any) {
		reasons = append(reasons, str(r))
	}
	rebuilt := attest.Result{
		NodeID: str(att["node_id"]), Verdict: attest.Verdict(str(att["verdict"])), Reasons: reasons,
		Nonce: str(att["nonce"]), MeasurementsDigest: str(att["measurements_digest"]),
		ReferenceVersion: str(att["reference_version"]), AttestedAt: signedAt,
	}

	if !ed25519.Verify(pub, attest.ResultMessage(rebuilt), sig) {
		t.Fatal("signature does not verify from the JSON fields and the published key")
	}
	rebuilt.Reasons = slices.Clone(reasons)[:1]
	if ed25519.Verify(pub, attest.ResultMessage(rebuilt), sig) {
		t.Error("signature verifies after dropping a reason")
	}
}

func TestGetAttestationPassHasEmptyReasonsList(t *testing.T) {
	res := failedResult()
	res.Verdict, res.Reasons = attest.VerdictPass, nil
	srv := newTestServer(t, newFakeStore(), withAttestations(signedAttestations(t, res)))

	_, body := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/attestation", "")

	if reasons, ok := body["reasons"].([]any); !ok || len(reasons) != 0 {
		t.Errorf("reasons = %#v, want an empty JSON array", body["reasons"])
	}
}

func TestGetAttestationRendersTimeInUTC(t *testing.T) {
	res := failedResult()
	res.AttestedAt = attestedAt.In(time.FixedZone("CEST", 2*3600))
	srv := newTestServer(t, newFakeStore(), withAttestations(signedAttestations(t, res)))

	_, body := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/attestation", "")

	if body["attested_at"] != "2030-06-01T12:00:05Z" {
		t.Errorf("attested_at = %v, want UTC", body["attested_at"])
	}
}

func TestGetAttestationMissingIs404(t *testing.T) {
	fs := newFakeStore()
	fs.nodes["gpu-001"] = node.Node{ID: "gpu-001", Pod: "p1", Rack: "r1", State: node.StateAvailable}
	srv := newTestServer(t, fs)

	for _, id := range []string{"gpu-001", "ghost"} {
		if status, body := do(t, srv, http.MethodGet, "/v1/nodes/"+id+"/attestation", ""); status != http.StatusNotFound {
			t.Errorf("GET attestation for %s = %d %v, want 404", id, status, body)
		}
	}
}

func TestGetAttestationFailureHidesDetails(t *testing.T) {
	fa := newFakeAttestations()
	fa.err = fmt.Errorf("connection reset: password=hunter2")
	srv := newTestServer(t, newFakeStore(), withAttestations(fa))

	status, body := do(t, srv, http.MethodGet, "/v1/nodes/gpu-001/attestation", "")

	if status != http.StatusInternalServerError || body["error"] != "internal error" {
		t.Fatalf("failure = %d %v, want generic 500", status, body)
	}
}

func TestAttestationKeys(t *testing.T) {
	fa := signedAttestations(t)
	srv := newTestServer(t, newFakeStore(), withAttestations(fa))

	status, body := do(t, srv, http.MethodGet, "/v1/attestation/keys", "")

	if status != http.StatusOK {
		t.Fatalf("status = %d %v, want 200", status, body)
	}
	want := map[string]any{"keys": []any{map[string]any{
		"alg":        "Ed25519",
		"public_key": base64.StdEncoding.EncodeToString(fa.pub),
	}}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

func TestNewHandlerRequiresAttestations(t *testing.T) {
	deps := testDeps(newFakeStore())
	deps.Attestations = nil
	defer func() {
		if recover() == nil {
			t.Error("NewHandler without Attestations did not panic")
		}
	}()
	NewHandler(deps)
}

func mustB64(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(str(v))
	if err != nil {
		t.Fatalf("decode %v: %v", v, err)
	}
	return raw
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
