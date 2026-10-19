package deployagent

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
)

const evidenceBody = `{
	"node_id": "node-001",
	"nonce": "` + testNonce + `",
	"pcrs": {"0": "aa", "1": "bb", "4": "cc"},
	"event_log": [{"pcr": 0, "component": "firmware", "name": "bios-2.4.1", "sha256": "dd"}],
	"quote_sig": "c2ln",
	"gpus": [{"index": 0, "vbios": "96.00.9F.00.01", "cc_mode": "on",
	          "device_pub": "cHVi", "device_cert_sig": "Y2VydA==", "report_sig": "cmVwb3J0"}],
	"unknown_field": true
}`

func TestIdentity(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, `{"ak_pub": "a2V5"}`)

	got, err := c.Identity(t.Context(), testSystem)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a2V5" {
		t.Errorf("Identity = %q, want a2V5", got)
	}
	req := rec.only(t)
	assertRequest(t, req, http.MethodGet, "/agent/node-001/identity")
	if req.Body != "" || req.ContentType != "" {
		t.Errorf("GET sent body %q with content type %q", req.Body, req.ContentType)
	}
}

func TestIdentityRejectsMissingKey(t *testing.T) {
	for name, body := range map[string]string{
		"empty object": `{}`,
		"empty key":    `{"ak_pub": ""}`,
		"wrong type":   `{"ak_pub": 5}`,
		"not json":     `<html></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			_, err := c.Identity(t.Context(), testSystem)
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, ErrTransient) {
				t.Errorf("a bad body is not transient: %v", err)
			}
		})
	}
}

func TestEvidence(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, evidenceBody)

	got, err := c.Evidence(t.Context(), testSystem, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	want := attest.Evidence{
		NodeID:   "node-001",
		Nonce:    testNonce,
		PCRs:     map[string]string{"0": "aa", "1": "bb", "4": "cc"},
		EventLog: []attest.Event{{PCR: 0, Component: attest.ComponentFirmware, Name: "bios-2.4.1", SHA256: "dd"}},
		QuoteSig: "c2ln",
		GPUs: []attest.GPUReport{{
			Index: 0, VBIOS: "96.00.9F.00.01", CCMode: "on",
			DevicePub: "cHVi", DeviceCertSig: "Y2VydA==", ReportSig: "cmVwb3J0",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Evidence = %+v, want %+v", got, want)
	}
	req := rec.only(t)
	assertRequest(t, req, http.MethodGet, "/agent/node-001/attestation")
	if req.RawQuery != "nonce="+testNonce {
		t.Errorf("query = %q, want nonce=%s", req.RawQuery, testNonce)
	}
}

func TestEvidenceEscapesNonce(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, evidenceBody)

	if _, err := c.Evidence(t.Context(), testSystem, "a&b=c d#e"); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.only(t).RawQuery, "nonce=a%26b%3Dc+d%23e"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

func TestEvidencePoweredOffIsPlainError(t *testing.T) {
	c, _ := newStub(t, http.StatusConflict, "")

	_, err := c.Evidence(t.Context(), testSystem, testNonce)
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []error{ErrTransient, ErrNotFound, ErrUnauthorized} {
		if errors.Is(err, s) {
			t.Errorf("409 must not map to %v: %v", s, err)
		}
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error %q lacks the status", err)
	}
}

func TestEvidenceRejectsMalformedBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      "",
		"truncated":  `{"node_id": "node-001", `,
		"wrong type": `{"node_id": 5}`,
		"array":      `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			if _, err := c.Evidence(t.Context(), testSystem, testNonce); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAttestationCallsRejectInvalidIDsWithoutRequest(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, evidenceBody)
	for _, id := range []string{"", ".", ".."} {
		if _, err := c.Identity(t.Context(), id); err == nil {
			t.Errorf("Identity(%q) = nil error", id)
		}
		if _, err := c.Evidence(t.Context(), id, testNonce); err == nil {
			t.Errorf("Evidence(%q) = nil error", id)
		}
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none: %+v", len(reqs), reqs)
	}
}
