package bmcsim

import (
	"encoding/base64"
	"slices"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
)

const referenceVersion = "2026-10-19.1"

func allowedByReference(ref attest.Reference, ev attest.Event) bool {
	return slices.Contains(ref.Components[ev.Component], attest.RefMeasure{Name: ev.Name, SHA256: ev.SHA256})
}

func TestReferenceValuesAcceptAnUntamperedNode(t *testing.T) {
	h := newHarness(t, nil)
	h.reset(t, testNode, "On")
	ref := ReferenceValues(testSeed, referenceVersion)
	ev := h.evidence(t)

	if ref.Version != referenceVersion {
		t.Fatalf("version = %q, want %q", ref.Version, referenceVersion)
	}
	for _, e := range ev.EventLog {
		if !allowedByReference(ref, e) {
			t.Errorf("reference values do not allow %+v", e)
		}
	}
	for _, g := range ev.GPUs {
		if !slices.Contains(ref.GPU.VBIOS, g.VBIOS) {
			t.Errorf("GPU %d VBIOS %q not allowed by %v", g.Index, g.VBIOS, ref.GPU.VBIOS)
		}
		if ref.GPU.RequireCCMode && g.CCMode != attest.CCModeOn {
			t.Errorf("GPU %d cc_mode = %q, reference requires on", g.Index, g.CCMode)
		}
	}
	if want := base64.StdEncoding.EncodeToString(h.vendorRoot(t)); ref.GPU.VendorRootPub != want {
		t.Fatalf("vendor_root_pub = %q, want the running simulator's %q", ref.GPU.VendorRootPub, want)
	}
}

func TestReferenceValuesRejectTamperedComponents(t *testing.T) {
	ref := ReferenceValues(testSeed, referenceVersion)
	for _, tc := range tamperEffects {
		if tc.event == "" {
			continue
		}
		h := newHarness(t, nil)
		h.reset(t, testNode, "On")
		h.tamper(t, tc.component)
		for _, e := range h.evidence(t).EventLog {
			if got, want := allowedByReference(ref, e), e.Component != tc.event; got != want {
				t.Errorf("%s tampered: %s allowed = %v, want %v", tc.component, e.Component, got, want)
			}
		}
	}
	if slices.Contains(ref.GPU.VBIOS, TamperedVBIOS) {
		t.Errorf("reference values allow the tampered VBIOS %q", TamperedVBIOS)
	}
}

func TestReferenceValuesListEveryMeasuredComponentOnce(t *testing.T) {
	ref := ReferenceValues(testSeed, referenceVersion)
	if len(ref.Components) != len(attest.Measured) {
		t.Fatalf("%d components, want %d", len(ref.Components), len(attest.Measured))
	}
	for _, m := range attest.Measured {
		if got := len(ref.Components[m.Component]); got != 1 {
			t.Errorf("%s has %d allowed entries, want 1", m.Component, got)
		}
	}
}
