package attestsvc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/store"
)

var nonceFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

func attestOK(t *testing.T, h *harness) attest.Result {
	t.Helper()
	res, err := h.svc.Attest(t.Context(), enrolledNode(t))
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	return res
}

func checkSignature(t *testing.T, h *harness, r attest.Result) {
	t.Helper()
	if !ed25519.Verify(h.svc.PublicKey(), attest.ResultMessage(r), r.Signature) {
		t.Errorf("signature does not verify over ResultMessage for %+v", r)
	}
}

func TestAttestPassIsRecordedAndSigned(t *testing.T) {
	h := newHarness(t)

	res := attestOK(t, h)

	if res.Verdict != attest.VerdictPass || res.NodeID != testNodeID || res.ReferenceVersion != testVersion {
		t.Errorf("result = %+v", res)
	}
	if want := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC); !res.AttestedAt.Equal(want) || res.AttestedAt.Location() != time.UTC {
		t.Errorf("AttestedAt = %v, want %v truncated to seconds in UTC", res.AttestedAt, want)
	}
	checkSignature(t, h, res)
	if len(h.store.recorded) != 1 || !reflect.DeepEqual(h.store.recorded[0], res) {
		t.Errorf("recorded = %+v, want exactly the returned result %+v", h.store.recorded, res)
	}
}

func TestAttestFailVerdictIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.verdict = attest.Result{
		Verdict:            attest.VerdictFail,
		Reasons:            []string{"bootloader digest not in reference values", "gpu 3 vbios not allowed"},
		MeasurementsDigest: "ab12",
	}

	res := attestOK(t, h)

	if res.Verdict != attest.VerdictFail || !slices.Equal(res.Reasons, h.verdict.Reasons) {
		t.Errorf("result = %+v, want the fail verdict and reasons", res)
	}
	checkSignature(t, h, res)
	if len(h.store.recorded) != 1 || !reflect.DeepEqual(h.store.recorded[0], res) {
		t.Errorf("recorded = %+v, want the fail result", h.store.recorded)
	}
}

func TestAttestChallengesTheRightNode(t *testing.T) {
	h := newHarness(t)
	n := enrolledNode(t)

	res, err := h.svc.Attest(t.Context(), n)
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}

	if len(h.store.issued) != 1 {
		t.Fatalf("issued %d nonces, want 1", len(h.store.issued))
	}
	iss := h.store.issued[0]
	if iss.nodeID != n.ID || !nonceFormat.MatchString(iss.nonce) {
		t.Errorf("issued %+v, want 64 hex chars for %s", iss, n.ID)
	}
	if want := testNow.Add(DefaultNonceTTL); !iss.expires.Equal(want) {
		t.Errorf("nonce expires %v, want now+60s = %v", iss.expires, want)
	}
	if !reflect.DeepEqual(h.dialed, []string{n.BMC}) {
		t.Errorf("dialed %v, want the node's BMC %q", h.dialed, n.BMC)
	}
	if want := []evidenceCall{{n.ID, iss.nonce}}; !reflect.DeepEqual(h.agent.calls, want) {
		t.Errorf("agent calls = %v, want %v", h.agent.calls, want)
	}
	if want := []consumed{{n.ID, iss.nonce, testNow}}; !reflect.DeepEqual(h.store.consumed, want) {
		t.Errorf("consumed = %v, want %v", h.store.consumed, want)
	}
	if res.Nonce != iss.nonce {
		t.Errorf("result nonce = %q, want the issued %q", res.Nonce, iss.nonce)
	}
}

func TestAttestHandsVerifierItsInputs(t *testing.T) {
	h := newHarness(t)
	h.agent.evidence = attest.Evidence{NodeID: testNodeID, QuoteSig: "c2ln", PCRs: map[string]string{"0": "aa"}}
	n := enrolledNode(t)
	pub, _ := mustKey(t, 2)

	attestOK(t, h)

	if len(h.verify) != 1 {
		t.Fatalf("verify called %d times, want 1", len(h.verify))
	}
	call := h.verify[0]
	if !reflect.DeepEqual(call.evidence, h.agent.evidence) {
		t.Errorf("evidence = %+v, want the agent's %+v", call.evidence, h.agent.evidence)
	}
	want := attest.Expected{NodeID: n.ID, Nonce: h.store.issued[0].nonce, AKPub: pub}
	if !reflect.DeepEqual(call.expected, want) {
		t.Errorf("expected = %+v, want %+v", call.expected, want)
	}
	if !reflect.DeepEqual(call.ref, h.ref) {
		t.Errorf("reference = %+v, want %+v", call.ref, h.ref)
	}
}

func TestAttestOrdersTheSteps(t *testing.T) {
	h := newHarness(t)

	attestOK(t, h)

	want := []string{"issue", "fetch", "consume", "verify", "record"}
	if got := h.log.all(); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestAttestUsesAFreshNonceEachTime(t *testing.T) {
	h := newHarness(t)

	attestOK(t, h)
	attestOK(t, h)

	if len(h.store.issued) != 2 || h.store.issued[0].nonce == h.store.issued[1].nonce {
		t.Errorf("issued %+v, want two distinct nonces", h.store.issued)
	}
}

func TestAttestTransientAgentErrorPassesThrough(t *testing.T) {
	h := newHarness(t)
	h.agent.err = fmt.Errorf("%w: GET /agent/node-001/attestation: status 503", deployagent.ErrTransient)

	_, err := h.svc.Attest(t.Context(), enrolledNode(t))

	if !errors.Is(err, deployagent.ErrTransient) {
		t.Fatalf("err = %v, want deployagent.ErrTransient", err)
	}
	assertNothingConsumedOrRecorded(t, h)
}

func TestAttestOtherAgentErrorsAreErrorsNotVerdicts(t *testing.T) {
	cases := map[string]error{
		"powered off":  errors.New("GET /agent/node-001/attestation: status 409"),
		"not found":    deployagent.ErrNotFound,
		"unauthorized": deployagent.ErrUnauthorized,
	}
	for name, agentErr := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.err = agentErr

			_, err := h.svc.Attest(t.Context(), enrolledNode(t))

			if !errors.Is(err, agentErr) || errors.Is(err, deployagent.ErrTransient) {
				t.Fatalf("err = %v, want %v and not transient", err, agentErr)
			}
			assertNothingConsumedOrRecorded(t, h)
		})
	}
}

func TestAttestCallerCancellationIsNotTransient(t *testing.T) {
	h := newHarness(t)
	h.agent.err = fmt.Errorf("GET /agent/node-001/attestation: %w", context.Canceled)

	_, err := h.svc.Attest(t.Context(), enrolledNode(t))

	if !errors.Is(err, context.Canceled) || errors.Is(err, deployagent.ErrTransient) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
}

func assertNothingConsumedOrRecorded(t *testing.T, h *harness) {
	t.Helper()
	if len(h.store.consumed) != 0 || len(h.store.recorded) != 0 || len(h.verify) != 0 {
		t.Errorf("consumed %d, recorded %d, verified %d; want none after a fetch failure",
			len(h.store.consumed), len(h.store.recorded), len(h.verify))
	}
}

func TestAttestMalformedEnrolledKeyIsARecordedFailure(t *testing.T) {
	raw := func(n int) []byte { return bytes.Repeat([]byte{0xfb}, n) }
	cases := map[string]string{
		"empty":         "",
		"not base64":    "!!!not base64!!!",
		"url alphabet":  base64.URLEncoding.EncodeToString(raw(32)),
		"too short":     base64.StdEncoding.EncodeToString(raw(31)),
		"too long":      base64.StdEncoding.EncodeToString(raw(33)),
		"unpadded":      base64.RawStdEncoding.EncodeToString(raw(32)),
		"leading space": " " + base64.StdEncoding.EncodeToString(raw(32)),
		"hex":           hex.EncodeToString(raw(32)),
	}
	for name, ak := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			n := enrolledNode(t)
			n.AKPub = ak

			res, err := h.svc.Attest(t.Context(), n)

			if err != nil {
				t.Fatalf("Attest: %v", err)
			}
			wantReasons := []string{"enrolled attestation key invalid"}
			if res.Verdict != attest.VerdictFail || !slices.Equal(res.Reasons, wantReasons) ||
				res.NodeID != n.ID || res.ReferenceVersion != testVersion {
				t.Errorf("result = %+v, want a fail with %v", res, wantReasons)
			}
			checkSignature(t, h, res)
			if len(h.store.recorded) != 1 || !reflect.DeepEqual(h.store.recorded[0], res) {
				t.Errorf("recorded = %+v, want the failure", h.store.recorded)
			}
			if got := h.log.all(); !slices.Equal(got, []string{"record"}) {
				t.Errorf("steps = %v, want only a record: nothing to challenge", got)
			}
		})
	}
}

func TestAttestStoreFailuresStopTheAttestation(t *testing.T) {
	boom := errors.New("db down")
	cases := []struct {
		name      string
		setup     func(h *harness)
		wantSteps []string
	}{
		{"issue", func(h *harness) { h.store.issueErr = boom }, []string{"issue"}},
		{"consume", func(h *harness) { h.store.consumeErr = fmt.Errorf("%w: node-001", store.ErrNonceInvalid) }, []string{"issue", "fetch", "consume"}},
		{"record", func(h *harness) { h.store.recordErr = boom }, []string{"issue", "fetch", "consume", "verify", "record"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)

			res, err := h.svc.Attest(t.Context(), enrolledNode(t))

			if err == nil {
				t.Fatalf("Attest = %+v, want an error", res)
			}
			if !reflect.DeepEqual(res, attest.Result{}) {
				t.Errorf("result = %+v, want the zero value with an error", res)
			}
			if got := h.log.all(); !slices.Equal(got, tc.wantSteps) {
				t.Errorf("steps = %v, want %v", got, tc.wantSteps)
			}
		})
	}
}

func TestStoreErrorsKeepTheirCause(t *testing.T) {
	h := newHarness(t)
	h.store.consumeErr = fmt.Errorf("%w: node-001", store.ErrNonceInvalid)

	_, err := h.svc.Attest(t.Context(), enrolledNode(t))

	if !errors.Is(err, store.ErrNonceInvalid) || errors.Is(err, deployagent.ErrTransient) {
		t.Fatalf("err = %v, want store.ErrNonceInvalid and not transient", err)
	}
}

func TestSignatureCoversEveryResultField(t *testing.T) {
	h := newHarness(t)
	h.verdict = attest.Result{
		Verdict:            attest.VerdictFail,
		Reasons:            []string{"first reason", "second reason"},
		MeasurementsDigest: "ab12",
	}
	res := attestOK(t, h)
	checkSignature(t, h, res)

	mutations := map[string]func(*attest.Result){
		"node id":             func(r *attest.Result) { r.NodeID = "node-002" },
		"verdict":             func(r *attest.Result) { r.Verdict = attest.VerdictPass },
		"nonce":               func(r *attest.Result) { r.Nonce += "0" },
		"measurements digest": func(r *attest.Result) { r.MeasurementsDigest = "ab13" },
		"reference version":   func(r *attest.Result) { r.ReferenceVersion = "2026-10-20.1" },
		"attested at":         func(r *attest.Result) { r.AttestedAt = r.AttestedAt.Add(time.Second) },
		"reason edited":       func(r *attest.Result) { r.Reasons = []string{"first reason", "other reason"} },
		"reason added":        func(r *attest.Result) { r.Reasons = append(slices.Clone(r.Reasons), "third reason") },
		"reason dropped":      func(r *attest.Result) { r.Reasons = r.Reasons[:1] },
		"reasons reordered":   func(r *attest.Result) { r.Reasons = []string{"second reason", "first reason"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := res
			tampered.Reasons = slices.Clone(res.Reasons)
			mutate(&tampered)
			if ed25519.Verify(h.svc.PublicKey(), attest.ResultMessage(tampered), res.Signature) {
				t.Errorf("signature still verifies after changing %s", name)
			}
		})
	}
}

func TestSignatureIsFromTheConfiguredKeyOnly(t *testing.T) {
	h := newHarness(t)
	res := attestOK(t, h)

	otherPub, _ := mustKey(t, 9)
	if ed25519.Verify(otherPub, attest.ResultMessage(res), res.Signature) {
		t.Error("signature verifies under an unrelated key")
	}
}

func TestLatestReturnsTheStoredResult(t *testing.T) {
	h := newHarness(t)
	h.store.latest = attest.Result{NodeID: testNodeID, Verdict: attest.VerdictPass, Nonce: "n"}

	got, err := h.svc.Latest(t.Context(), testNodeID)

	if err != nil || !reflect.DeepEqual(got, h.store.latest) {
		t.Errorf("Latest = %+v, %v; want %+v", got, err, h.store.latest)
	}
}

func TestLatestKeepsNotFound(t *testing.T) {
	h := newHarness(t)
	h.store.latestErr = fmt.Errorf("%w: ghost", store.ErrAttestationNotFound)

	_, err := h.svc.Latest(t.Context(), "ghost")

	if !errors.Is(err, store.ErrAttestationNotFound) {
		t.Errorf("err = %v, want store.ErrAttestationNotFound", err)
	}
}

func TestPublicKeyMatchesTheSigningKeyAndIsACopy(t *testing.T) {
	h := newHarness(t)
	want, _ := mustKey(t, 1)

	got := h.svc.PublicKey()
	if !got.Equal(want) {
		t.Fatalf("PublicKey = %x, want %x", got, want)
	}
	got[0] ^= 0xff
	if !h.svc.PublicKey().Equal(want) {
		t.Error("mutating the returned key changed the service's key")
	}
}
