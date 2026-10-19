package bmcsim

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
)

const (
	testSeed      = 1 // the harness seed
	testNode      = "node-001"
	otherNode     = "node-002"
	nonceA        = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	nonceB        = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	attestationOf = "/agent/node-001/attestation?nonce="
)

func (h *harness) reset(t *testing.T, id, resetType string) {
	t.Helper()
	h.mustDo(t, http.StatusNoContent, "POST",
		"/redfish/v1/Systems/"+id+"/Actions/ComputerSystem.Reset", `{"ResetType": "`+resetType+`"}`)
}

func (h *harness) evidenceOf(t *testing.T, id, nonce string) attest.Evidence {
	t.Helper()
	rec := h.mustDo(t, http.StatusOK, "GET", "/agent/"+id+"/attestation?nonce="+nonce, "")
	var ev attest.Evidence
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatalf("evidence is not JSON: %v: %s", err, rec.Body)
	}
	return ev
}

func (h *harness) evidence(t *testing.T) attest.Evidence {
	t.Helper()
	return h.evidenceOf(t, testNode, nonceA)
}

func (h *harness) tamper(t *testing.T, component string) {
	t.Helper()
	h.mustDo(t, http.StatusNoContent, "POST", "/sim/systems/"+testNode+"/tamper", `{"component": "`+component+`"}`)
}

func (h *harness) enrolledAK(t *testing.T) ed25519.PublicKey {
	t.Helper()
	var id struct {
		AKPub string `json:"ak_pub"`
	}
	rec := h.mustDo(t, http.StatusOK, "GET", "/agent/"+testNode+"/identity", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatal(err)
	}
	return decodeKey(t, id.AKPub)
}

func (h *harness) vendorRoot(t *testing.T) ed25519.PublicKey {
	t.Helper()
	var v struct {
		VendorRootPub string `json:"vendor_root_pub"`
	}
	rec := h.mustDo(t, http.StatusOK, "GET", "/sim/vendor-root", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return decodeKey(t, v.VendorRootPub)
}

func (h *harness) wipeCycle(t *testing.T) {
	t.Helper()
	h.mustDo(t, http.StatusNoContent, "PUT", "/agent/"+testNode+"/task", `{"task": "wipe"}`)
	h.mustDo(t, http.StatusNoContent, "PATCH", "/redfish/v1/Systems/"+testNode,
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)
	h.reset(t, testNode, "ForceRestart")
	h.clock.Advance(testRunTime)
	wantJSON(t, h.mustDo(t, http.StatusOK, "GET", "/agent/"+testNode+"/status", ""),
		`{"task": "wipe", "phase": "succeeded"}`)
}

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("%q is not standard base64: %v", s, err)
	}
	return b
}

func decodeKey(t *testing.T, s string) ed25519.PublicKey {
	t.Helper()
	b := decodeB64(t, s)
	if len(b) != ed25519.PublicKeySize {
		t.Fatalf("key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return b
}

func quoteSigned(t *testing.T, ev attest.Evidence, key ed25519.PublicKey) bool {
	t.Helper()
	pcrs, err := attest.ParsePCRs(ev.PCRs)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.Verify(key, attest.QuoteMessage(ev.NodeID, ev.Nonce, pcrs), decodeB64(t, ev.QuoteSig))
}

// checkEvidence asserts everything a verifier relies on: the quote is signed
// by quoteKey, the log replays to the quoted PCRs, and the GPU chain verifies.
func checkEvidence(t *testing.T, ev attest.Evidence, nodeID, nonce string, quoteKey, vendorRoot ed25519.PublicKey) {
	t.Helper()
	if ev.NodeID != nodeID || ev.Nonce != nonce {
		t.Fatalf("node_id/nonce = %q/%q, want %q/%q", ev.NodeID, ev.Nonce, nodeID, nonce)
	}
	quoted, err := attest.ParsePCRs(ev.PCRs)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := attest.ReplayPCRs(ev.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(quoted, replayed) {
		t.Fatalf("quoted PCRs %v differ from replayed event log %v", ev.PCRs, replayed)
	}
	if !quoteSigned(t, ev, quoteKey) {
		t.Fatal("quote signature does not verify under the expected AK")
	}
	if len(ev.GPUs) != attest.GPUsPerNode {
		t.Fatalf("GPUs = %d, want %d", len(ev.GPUs), attest.GPUsPerNode)
	}
	for i, g := range ev.GPUs {
		if g.Index != i {
			t.Fatalf("GPU %d has index %d", i, g.Index)
		}
		devicePub := decodeKey(t, g.DevicePub)
		if !ed25519.Verify(vendorRoot, attest.GPUCertMessage(nodeID, i, g.DevicePub), decodeB64(t, g.DeviceCertSig)) {
			t.Fatalf("GPU %d: device cert does not verify under the vendor root", i)
		}
		report := attest.GPUReportMessage(nodeID, nonce, i, g.VBIOS, g.CCMode)
		if !ed25519.Verify(devicePub, report, decodeB64(t, g.ReportSig)) {
			t.Fatalf("GPU %d: report does not verify under its device key", i)
		}
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}

func TestIdentityReturnsEnrolledAK(t *testing.T) {
	h := newHarness(t, nil)
	want := base64.StdEncoding.EncodeToString(AKPublic(testSeed, testNode))
	wantJSON(t, h.mustDo(t, http.StatusOK, "GET", "/agent/"+testNode+"/identity", ""), `{"ak_pub": "`+want+`"}`)

	h.tamper(t, "ak")
	wantJSON(t, h.mustDo(t, http.StatusOK, "GET", "/agent/"+testNode+"/identity", ""), `{"ak_pub": "`+want+`"}`)
}

func TestVendorRootEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.vendorRoot(t); !got.Equal(VendorRootPublic(testSeed)) {
		t.Fatal("/sim/vendor-root does not return the seed's vendor root")
	}
}

func TestEvidenceVerifiesWithTheSharedEncoders(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")

	ev := h.evidence(t)
	checkEvidence(t, ev, testNode, nonceA, h.enrolledAK(t), h.vendorRoot(t))
}

func TestEvidenceDefaultContents(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	ev := h.evidence(t)

	wantLog := []attest.Event{
		{PCR: 0, Component: "firmware", Name: "bios-2.4.1"},
		{PCR: 1, Component: "bmc", Name: "bmc-1.9.3"},
		{PCR: 4, Component: "bootloader", Name: "grub-2.12"},
		{PCR: 4, Component: "kernel", Name: "inspect-ramdisk-6.8.0"},
	}
	for i := range wantLog {
		wantLog[i].SHA256 = hex.EncodeToString(attest.ComponentDigest(wantLog[i].Name))
	}
	if !reflect.DeepEqual(ev.EventLog, wantLog) {
		t.Fatalf("event log = %+v, want %+v", ev.EventLog, wantLog)
	}
	if got := slices.Sorted(maps.Keys(ev.PCRs)); !slices.Equal(got, []string{"0", "1", "4"}) {
		t.Fatalf("PCR indices = %v, want [0 1 4]", got)
	}
	for _, g := range ev.GPUs {
		if g.VBIOS != "96.00.9F.00.01" || g.CCMode != "on" {
			t.Fatalf("GPU %d: vbios/cc_mode = %q/%q, want 96.00.9F.00.01/on", g.Index, g.VBIOS, g.CCMode)
		}
	}
}

func TestEvidenceWireShape(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	body := h.mustDo(t, http.StatusOK, "GET", attestationOf+nonceA, "").Body.Bytes()

	if got, want := jsonKeys(t, body), []string{"event_log", "gpus", "node_id", "nonce", "pcrs", "quote_sig"}; !slices.Equal(got, want) {
		t.Fatalf("evidence keys = %v, want %v", got, want)
	}
	var raw struct {
		EventLog []json.RawMessage `json:"event_log"`
		GPUs     []json.RawMessage `json:"gpus"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := jsonKeys(t, raw.EventLog[0]), []string{"component", "name", "pcr", "sha256"}; !slices.Equal(got, want) {
		t.Fatalf("event keys = %v, want %v", got, want)
	}
	wantGPU := []string{"cc_mode", "device_cert_sig", "device_pub", "index", "report_sig", "vbios"}
	if got := jsonKeys(t, raw.GPUs[0]); !slices.Equal(got, wantGPU) {
		t.Fatalf("gpu keys = %v, want %v", got, wantGPU)
	}
}

func TestEvidenceIsBoundToTheNonce(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	a, again, b := h.evidenceOf(t, testNode, nonceA), h.evidenceOf(t, testNode, nonceA), h.evidenceOf(t, testNode, nonceB)

	if !reflect.DeepEqual(a, again) {
		t.Fatal("same nonce gave different evidence")
	}
	checkEvidence(t, b, testNode, nonceB, h.enrolledAK(t), h.vendorRoot(t))
	if a.QuoteSig == b.QuoteSig {
		t.Fatal("quote signature does not depend on the nonce")
	}
	if a.GPUs[0].ReportSig == b.GPUs[0].ReportSig {
		t.Fatal("GPU report signature does not depend on the nonce")
	}
	if a.GPUs[0].DeviceCertSig != b.GPUs[0].DeviceCertSig {
		t.Fatal("device cert signature must not depend on the nonce")
	}
}

func TestEachNodeHasItsOwnIdentity(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	h.reset(t, otherNode, "On")
	a, b := h.evidenceOf(t, testNode, nonceA), h.evidenceOf(t, otherNode, nonceA)

	checkEvidence(t, b, otherNode, nonceA, AKPublic(testSeed, otherNode), h.vendorRoot(t))
	if quoteSigned(t, b, AKPublic(testSeed, testNode)) {
		t.Fatal("node-002 quote verifies under node-001's AK")
	}
	if a.GPUs[0].DevicePub == b.GPUs[0].DevicePub {
		t.Fatal("nodes share a GPU device key")
	}
}

type tamperEffect struct {
	component string
	event     attest.Component // the event that becomes unknown, if any
	vbios     bool
	ccOff     bool
	ak        bool
}

var tamperEffects = []tamperEffect{
	{component: "firmware", event: attest.ComponentFirmware},
	{component: "bmc", event: attest.ComponentBMC},
	{component: "bootloader", event: attest.ComponentBootloader},
	{component: "kernel", event: attest.ComponentKernel},
	{component: "gpu_vbios", vbios: true},
	{component: "cc_mode", ccOff: true},
	{component: "ak", ak: true},
}

func pcrIndex(c attest.Component) string {
	for _, m := range attest.Measured {
		if m.Component == c {
			return strconv.Itoa(m.PCR)
		}
	}
	return ""
}

func TestTamperChangesExactlyWhatTheDocSays(t *testing.T) {
	genuineAK := AKPublic(testSeed, testNode)
	forgedAK := publicOf(specKey("gpu-fleet/bmcsim/ak-tampered/1/node-001"))

	for _, tc := range tamperEffects {
		t.Run(tc.component, func(t *testing.T) {
			h := newHarness(t, nil)
			h.reset(t, testNode, "On")
			base := h.evidence(t)
			h.tamper(t, tc.component)
			got := h.evidence(t)

			assertEventsTampered(t, base, got, tc.event)
			assertPCRsChanged(t, base, got, tc.event)
			assertGPUs(t, base, got, tc)
			assertQuoteSigner(t, got, genuineAK, forgedAK, tc.ak)

			signer := genuineAK
			if tc.ak {
				signer = forgedAK
			}
			checkEvidence(t, got, testNode, nonceA, signer, h.vendorRoot(t))
		})
	}
}

func assertEventsTampered(t *testing.T, base, got attest.Evidence, tampered attest.Component) {
	t.Helper()
	if len(got.EventLog) != len(base.EventLog) {
		t.Fatalf("event log has %d events, want %d", len(got.EventLog), len(base.EventLog))
	}
	for i, ev := range got.EventLog {
		want := base.EventLog[i]
		if want.Component == tampered {
			want.Name, want.SHA256 = "unknown", hex.EncodeToString(attest.TamperedDigest(tampered))
		}
		if ev != want {
			t.Errorf("event %d = %+v, want %+v", i, ev, want)
		}
	}
}

func assertPCRsChanged(t *testing.T, base, got attest.Evidence, tampered attest.Component) {
	t.Helper()
	if !slices.Equal(slices.Sorted(maps.Keys(got.PCRs)), slices.Sorted(maps.Keys(base.PCRs))) {
		t.Fatalf("PCR indices changed: %v vs %v", got.PCRs, base.PCRs)
	}
	for idx, val := range got.PCRs {
		wantChanged := tampered != "" && idx == pcrIndex(tampered)
		if changed := val != base.PCRs[idx]; changed != wantChanged {
			t.Errorf("PCR %s changed = %v, want %v", idx, changed, wantChanged)
		}
	}
}

func assertGPUs(t *testing.T, base, got attest.Evidence, tc tamperEffect) {
	t.Helper()
	if !tc.vbios && !tc.ccOff {
		if !reflect.DeepEqual(got.GPUs, base.GPUs) {
			t.Fatal("GPU evidence changed without a gpu_vbios or cc_mode tamper")
		}
		return
	}
	for i, g := range got.GPUs {
		want := base.GPUs[i]
		if tc.vbios {
			want.VBIOS = "99.99.99.99.99"
		}
		if tc.ccOff {
			want.CCMode = attest.CCModeOff
		}
		want.ReportSig = g.ReportSig
		if g != want {
			t.Errorf("GPU %d = %+v, want only the VBIOS and report signature changed from %+v", i, g, base.GPUs[i])
		}
		if g.ReportSig == base.GPUs[i].ReportSig {
			t.Errorf("GPU %d: report was not re-signed for the new VBIOS", i)
		}
	}
}

func assertQuoteSigner(t *testing.T, got attest.Evidence, genuine, forged ed25519.PublicKey, akTampered bool) {
	t.Helper()
	if signed := quoteSigned(t, got, genuine); signed == akTampered {
		t.Errorf("quote verifies under the enrolled AK = %v, want %v", signed, !akTampered)
	}
	if signed := quoteSigned(t, got, forged); signed != akTampered {
		t.Errorf("quote verifies under the forged AK = %v, want %v", signed, akTampered)
	}
}

func TestTamperAccumulates(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	h.tamper(t, "firmware")
	h.tamper(t, "kernel")
	h.tamper(t, "kernel")

	var unknown []attest.Component
	for _, ev := range h.evidence(t).EventLog {
		if ev.Name == "unknown" {
			unknown = append(unknown, ev.Component)
		}
	}
	if want := []attest.Component{"firmware", "kernel"}; !slices.Equal(unknown, want) {
		t.Fatalf("tampered components = %v, want %v", unknown, want)
	}
}

func TestTamperSurvivesPowerCyclesAndWipes(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	base := h.evidence(t)
	h.tamper(t, "bootloader")
	h.tamper(t, "gpu_vbios")
	tampered := h.evidence(t)

	h.reset(t, testNode, "ForceOff")
	h.reset(t, testNode, "On")
	h.wipeCycle(t)
	h.reset(t, testNode, "ForceRestart")

	if got := h.evidence(t); !reflect.DeepEqual(got, tampered) {
		t.Fatal("tampered evidence changed across power cycles and a wipe")
	}
	if reflect.DeepEqual(tampered, base) {
		t.Fatal("tamper had no effect")
	}
}

func TestDeleteTamperClearsEverything(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	base := h.evidence(t)
	for _, tc := range tamperEffects {
		h.tamper(t, tc.component)
	}
	if reflect.DeepEqual(h.evidence(t), base) {
		t.Fatal("tamper had no effect")
	}

	h.mustDo(t, http.StatusNoContent, "DELETE", "/sim/systems/"+testNode+"/tamper", "")
	if !reflect.DeepEqual(h.evidence(t), base) {
		t.Fatal("evidence differs from the untampered baseline after DELETE")
	}
	h.mustDo(t, http.StatusNoContent, "DELETE", "/sim/systems/"+testNode+"/tamper", "")
}

func TestTamperIsPerNode(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	h.reset(t, otherNode, "On")
	base := h.evidenceOf(t, otherNode, nonceA)

	h.tamper(t, "firmware")
	if got := h.evidenceOf(t, otherNode, nonceA); !reflect.DeepEqual(got, base) {
		t.Fatal("tampering node-001 changed node-002's evidence")
	}
}

func TestAttestationRequiresPowerOn(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusConflict, "GET", attestationOf+nonceA, "")

	h.reset(t, testNode, "On")
	h.evidence(t)
	h.reset(t, testNode, "ForceOff")
	h.mustDo(t, http.StatusConflict, "GET", attestationOf+nonceA, "")
	h.mustDo(t, http.StatusOK, "GET", "/agent/"+testNode+"/identity", "")
}

func TestBadNonceIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	valid := nonceA
	bad := map[string]string{
		"missing":   "/agent/node-001/attestation",
		"empty":     attestationOf,
		"short":     attestationOf + valid[:63],
		"long":      attestationOf + valid + "0",
		"uppercase": attestationOf + "AA" + valid[2:],
		"not hex":   attestationOf + "zz" + valid[2:],
		"padded":    attestationOf + "%20" + valid[1:],
		"0x prefix": attestationOf + "0x" + valid[2:],
	}
	for name, path := range bad {
		t.Run(name, func(t *testing.T) {
			h.mustDo(t, http.StatusBadRequest, "GET", path, "")
		})
	}
}

func TestBadNonceIsRejectedWhilePoweredOff(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusBadRequest, "GET", attestationOf+"abc", "")
}

func TestBadTamperRequests(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	base := h.evidence(t)
	path := "/sim/systems/" + testNode + "/tamper"
	bodies := map[string]string{
		"unknown component":   `{"component": "cpu"}`,
		"wrong case":          `{"component": "Firmware"}`,
		"empty component":     `{"component": ""}`,
		"missing component":   `{}`,
		"empty body":          ``,
		"malformed":           `{"component": `,
		"unknown field":       `{"component": "bmc", "persist": true}`,
		"component is number": `{"component": 4}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			h.mustDo(t, http.StatusBadRequest, "POST", path, body)
		})
	}
	if !reflect.DeepEqual(h.evidence(t), base) {
		t.Fatal("a rejected tamper request changed the evidence")
	}
}

func TestAttestationEndpointsSubjectToErrorInjection(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 1 })
	for _, path := range []string{"/agent/" + testNode + "/identity", attestationOf + nonceA} {
		h.mustDo(t, http.StatusServiceUnavailable, "GET", path, "")
	}
}

func TestVendorRootAndTamperExemptFromErrorInjection(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 1 })
	h.vendorRoot(t)
	h.tamper(t, "kernel")
	h.mustDo(t, http.StatusNoContent, "DELETE", "/sim/systems/"+testNode+"/tamper", "")
}
