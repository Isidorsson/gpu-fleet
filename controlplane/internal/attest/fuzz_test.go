package attest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func FuzzVerify(f *testing.F) {
	r := newRig(f)
	seeds := []Evidence{
		r.evidence(),
		r.evidence(withTampered(ComponentFirmware), withVBIOS(3, "99.99.99.99.99")),
		r.evidence(withoutGPU(7), withCCMode(1, CCModeOff)),
		{},
	}
	for _, ev := range seeds {
		data, err := json.Marshal(ev)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, []byte(r.expected().AKPub), r.reference().GPU.VendorRootPub)
	}

	f.Fuzz(func(t *testing.T, evJSON, akPub []byte, vendorRoot string) {
		var ev Evidence
		if json.Unmarshal(evJSON, &ev) != nil {
			t.Skip()
		}
		want, ref := r.expected(), r.reference()
		want.AKPub = akPub
		ref.GPU.VendorRootPub = vendorRoot
		assertWellFormed(t, ev, want, Verify(ev, want, ref))
	})
}

func assertWellFormed(t *testing.T, ev Evidence, want Expected, res Result) {
	t.Helper()
	if (res.Verdict == VerdictPass) != (len(res.Reasons) == 0) {
		t.Fatalf("verdict %q with reasons %q", res.Verdict, res.Reasons)
	}
	if res.Verdict != VerdictPass && res.Verdict != VerdictFail {
		t.Fatalf("unknown verdict %q", res.Verdict)
	}
	if !slices.IsSorted(res.Reasons) || len(slices.Compact(slices.Clone(res.Reasons))) != len(res.Reasons) {
		t.Fatalf("reasons not sorted and unique: %q", res.Reasons)
	}
	for _, reason := range res.Reasons {
		if strings.ContainsAny(reason, "\r\n") || reason == "" {
			t.Fatalf("unsafe reason %q", reason)
		}
	}
	if res.NodeID != want.NodeID || res.Nonce != want.Nonce {
		t.Fatalf("result does not echo the expected node and nonce: %+v", res)
	}
	if res.Verdict == VerdictPass && (ev.NodeID != want.NodeID || ev.Nonce != want.Nonce || len(ev.GPUs) != GPUsPerNode) {
		t.Fatalf("passed with evidence that does not match: %+v", ev)
	}
}
