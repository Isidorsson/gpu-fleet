package attest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	rNonce       = "nonce mismatch"
	rNodeID      = "node_id mismatch"
	rAKInvalid   = "enrolled attestation key invalid"
	rPCRs        = "quoted pcrs malformed"
	rQuoteSig    = "quote signature invalid"
	rReplay      = "event log does not replay to quoted pcrs"
	rLogShape    = "event log missing or out of order"
	rVendorRoot  = "reference vendor root invalid"
	rFirmwareRef = "firmware digest not in reference values"
	rBMCRef      = "bmc digest not in reference values"
	rBootRef     = "bootloader digest not in reference values"
	rKernelRef   = "kernel digest not in reference values"
)

type scenario struct {
	name    string
	opts    []option
	tweak   func(ev *Evidence, want *Expected, ref *Reference)
	reasons []string
}

func runScenarios(t *testing.T, cases []scenario) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			ev, want, ref := r.evidence(c.opts...), r.expected(), r.reference()
			if c.tweak != nil {
				c.tweak(&ev, &want, &ref)
			}
			assertReasons(t, Verify(ev, want, ref), c.reasons...)
		})
	}
}

func assertReasons(t *testing.T, got Result, want ...string) {
	t.Helper()
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got.Reasons, want) {
		t.Fatalf("reasons:\n got  %q\n want %q", got.Reasons, want)
	}
	wantVerdict := VerdictFail
	if len(want) == 0 {
		wantVerdict = VerdictPass
	}
	if got.Verdict != wantVerdict {
		t.Fatalf("verdict = %q, want %q", got.Verdict, wantVerdict)
	}
	if got.Reasons == nil {
		t.Fatal("Reasons is nil, want a non-nil slice")
	}
	if !slices.IsSorted(got.Reasons) || len(slices.Compact(slices.Clone(got.Reasons))) != len(got.Reasons) {
		t.Fatalf("reasons not sorted and unique: %q", got.Reasons)
	}
}

func gpuReasons(msg string, indices ...int) []string {
	out := make([]string, 0, len(indices))
	for _, i := range indices {
		out = append(out, fmt.Sprintf("gpu %d: %s", i, msg))
	}
	return out
}

func allGPUs() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7} }

func corruptB64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		t.Fatalf("fixture value is not base64: %q", s)
	}
	b[0] ^= 0x01
	return b64(b)
}

func TestValidEvidencePasses(t *testing.T) {
	r := newRig(t)
	res := Verify(r.evidence(), r.expected(), r.reference())
	assertReasons(t, res)
	pcrs, err := ReplayPCRs(r.evidence().EventLog)
	if err != nil {
		t.Fatal(err)
	}
	if want := MeasurementsDigest(pcrs); res.MeasurementsDigest != want {
		t.Errorf("MeasurementsDigest = %q, want %q", res.MeasurementsDigest, want)
	}
	if res.ReferenceVersion != testRefVer || res.Nonce != testNonce || res.NodeID != testNodeID {
		t.Errorf("echoed fields wrong: %+v", res)
	}
	if !res.AttestedAt.IsZero() || res.Signature != nil {
		t.Errorf("Verify must leave AttestedAt and Signature to the caller: %+v", res)
	}
}

func TestIdentityChecks(t *testing.T) {
	runScenarios(t, []scenario{
		{name: "nonce claim differs", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.Nonce = testOldNonce
		}, reasons: []string{rNonce}},
		{name: "node_id claim differs", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.NodeID = "node-002"
		}, reasons: []string{rNodeID}},
		{name: "both claims differ", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.NodeID, ev.Nonce = "node-002", testOldNonce
		}, reasons: []string{rNodeID, rNonce}},
	})
}

func TestQuoteSignature(t *testing.T) {
	attacker := testKey("attacker")
	runScenarios(t, []scenario{
		{name: "signed by another key", opts: []option{withAK(attacker)}, reasons: []string{rQuoteSig}},
		{name: "enrolled key differs from the evidence key", tweak: func(_ *Evidence, want *Expected, _ *Reference) {
			want.AKPub = pubOf(attacker)
		}, reasons: []string{rQuoteSig}},
		{name: "bit flipped", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.QuoteSig = corruptB64(t, ev.QuoteSig)
		}, reasons: []string{rQuoteSig}},
		{name: "not base64", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.QuoteSig = "!!!not base64!!!"
		}, reasons: []string{rQuoteSig}},
		{name: "empty", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.QuoteSig = ""
		}, reasons: []string{rQuoteSig}},
		{name: "wrong length", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.QuoteSig = b64(make([]byte, 10))
		}, reasons: []string{rQuoteSig}},
		{name: "enrolled key too short", tweak: func(_ *Evidence, want *Expected, _ *Reference) {
			want.AKPub = want.AKPub[:10]
		}, reasons: []string{rAKInvalid}},
		{name: "enrolled key missing", tweak: func(_ *Evidence, want *Expected, _ *Reference) {
			want.AKPub = nil
		}, reasons: []string{rAKInvalid}},
	})
}

func TestEventLogReplay(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "log edited but quote untouched",
			opts: []option{withTampered(ComponentFirmware)},
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[0] = componentEvent(0, ComponentFirmware, defaultNames[ComponentFirmware])
			},
			reasons: []string{rReplay},
		},
		{
			name: "log edited, pcrs recomputed, re-signed with another key",
			opts: []option{withTampered(ComponentBootloader)},
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[2] = componentEvent(4, ComponentBootloader, defaultNames[ComponentBootloader])
				newRig(t).requote(ev, testKey("attacker"))
			},
			reasons: []string{rQuoteSig},
		},
		{
			name: "log edited and pcrs recomputed but quote kept",
			opts: []option{withTampered(ComponentBootloader)},
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				sig := ev.QuoteSig
				ev.EventLog[2] = componentEvent(4, ComponentBootloader, defaultNames[ComponentBootloader])
				newRig(t).requote(ev, testKey("attacker"))
				ev.QuoteSig = sig
			},
			reasons: []string{rQuoteSig},
		},
		{
			name: "pcr 4 events swapped",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[2], ev.EventLog[3] = ev.EventLog[3], ev.EventLog[2]
			},
			reasons: []string{rLogShape, rReplay},
		},
		{
			name: "independent pcr events swapped",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[0], ev.EventLog[1] = ev.EventLog[1], ev.EventLog[0]
			},
			reasons: []string{rLogShape},
		},
		{
			name: "kernel event dropped",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog = ev.EventLog[:3]
			},
			reasons: []string{rLogShape, rReplay},
		},
		{
			name: "extra event appended",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog = append(ev.EventLog, ev.EventLog[2])
			},
			reasons: []string{rLogShape, rReplay},
		},
		{
			name: "empty log",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog = nil
			},
			reasons: []string{rLogShape, rReplay},
		},
		{
			name: "event on the wrong pcr",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[0].PCR = 2
			},
			reasons: []string{rLogShape, rReplay},
		},
		{
			name: "event labelled with an unmeasured component",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[1].Component = "bootkit"
			},
			reasons: []string{rLogShape},
		},
		{
			name: "event relabelled as another component",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[1].Component = ComponentFirmware
			},
			reasons: []string{rLogShape, rFirmwareRef},
		},
		{
			name: "garbage event digest",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[3].SHA256 = "zz"
			},
			reasons: []string{rReplay, rKernelRef},
		},
	})
}

func TestReferenceDigests(t *testing.T) {
	var cases []scenario
	for _, c := range []struct {
		component Component
		reason    string
	}{
		{ComponentFirmware, rFirmwareRef},
		{ComponentBMC, rBMCRef},
		{ComponentBootloader, rBootRef},
		{ComponentKernel, rKernelRef},
	} {
		cases = append(cases, scenario{
			name:    "tampered " + string(c.component),
			opts:    []option{withTampered(c.component)},
			reasons: []string{c.reason},
		})
	}
	runScenarios(t, append(cases,
		scenario{
			name:    "honest name on a tampered digest",
			opts:    []option{withTampered(ComponentFirmware)},
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.EventLog[0].Name = defaultNames[ComponentFirmware] },
			reasons: []string{rFirmwareRef},
		},
		scenario{
			name:  "lying name on an allowed digest",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.EventLog[0].Name = "unknown" },
		},
		scenario{
			name: "uppercase digest hex still compares by bytes",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.EventLog[0].SHA256 = strings.ToUpper(ev.EventLog[0].SHA256)
			},
		},
		scenario{
			name:    "reference lists nothing",
			tweak:   func(_ *Evidence, _ *Expected, ref *Reference) { ref.Components = nil },
			reasons: []string{rFirmwareRef, rBMCRef, rBootRef, rKernelRef},
		},
		scenario{
			name: "unparsable reference entries are ignored",
			tweak: func(_ *Evidence, _ *Expected, ref *Reference) {
				ref.Components[ComponentKernel] = append([]RefMeasure{{SHA256: "zz"}}, ref.Components[ComponentKernel]...)
			},
		},
	))
}

func TestFirmwareRollout(t *testing.T) {
	rollout := func(_ *Evidence, _ *Expected, ref *Reference) {
		ref.Components[ComponentFirmware] = append(ref.Components[ComponentFirmware], refMeasure("bios-2.5.0"))
	}
	runScenarios(t, []scenario{
		{name: "old firmware still accepted", tweak: rollout},
		{name: "new firmware accepted", opts: []option{withComponent(ComponentFirmware, "bios-2.5.0")}, tweak: rollout},
		{
			name:    "unlisted firmware rejected",
			opts:    []option{withComponent(ComponentFirmware, "bios-2.3.0")},
			tweak:   rollout,
			reasons: []string{rFirmwareRef},
		},
		{
			name:    "new firmware rejected before the rollout",
			opts:    []option{withComponent(ComponentFirmware, "bios-2.5.0")},
			reasons: []string{rFirmwareRef},
		},
	})
}

func TestReplayedEvidenceFails(t *testing.T) {
	staleNonce := append(gpuReasons("report signature invalid", allGPUs()...), rNonce, rQuoteSig)
	relabelledNonce := append(gpuReasons("report signature invalid", allGPUs()...), rQuoteSig)
	runScenarios(t, []scenario{
		{name: "evidence collected under an old nonce", opts: []option{withNonce(testOldNonce)}, reasons: staleNonce},
		{
			name:    "old evidence with its nonce claim rewritten",
			opts:    []option{withNonce(testOldNonce)},
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.Nonce = testNonce },
			reasons: relabelledNonce,
		},
	})
}

func TestNodeSwapFails(t *testing.T) {
	otherNode := []option{withNode("node-002"), withAK(testKey("ak-node-002"))}
	gpuSigs := append(gpuReasons("device certificate invalid", allGPUs()...), gpuReasons("report signature invalid", allGPUs()...)...)
	runScenarios(t, []scenario{
		{
			name:    "another node's evidence",
			opts:    otherNode,
			reasons: append(gpuSigs, rNodeID, rQuoteSig),
		},
		{
			name:    "another node's evidence relabelled",
			opts:    otherNode,
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.NodeID = testNodeID },
			reasons: append(gpuSigs, rQuoteSig),
		},
		{
			name:    "same key, node_id claim rewritten",
			opts:    []option{withNode("node-002")},
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.NodeID = testNodeID },
			reasons: append(gpuSigs, rQuoteSig),
		},
	})
}

func TestEveryFailureIsReported(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "many tampered parts",
		opts: []option{
			withTampered(ComponentFirmware), withTampered(ComponentKernel),
			withVBIOS(2, "99.99.99.99.99"), withCCMode(5, CCModeOff), withoutGPU(7),
		},
		tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.NodeID, ev.Nonce = "node-002", testOldNonce
		},
		reasons: []string{
			rNodeID, rNonce, rFirmwareRef, rKernelRef,
			"gpu 2: vbios 99.99.99.99.99 not allowed",
			"gpu 5: confidential computing mode off",
			"expected 8 gpus, got 7",
		},
	}})
}

func TestMalformedInputs(t *testing.T) {
	runScenarios(t, []scenario{
		{name: "zero value everything", tweak: func(ev *Evidence, want *Expected, ref *Reference) {
			*ev, *want, *ref = Evidence{}, Expected{}, Reference{}
		}, reasons: []string{rAKInvalid, rLogShape, rVendorRoot, "expected 8 gpus, got 0"}},
		{name: "nil pcrs", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs = nil }, reasons: []string{rQuoteSig, rReplay}},
		{name: "pcr value is not hex", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["0"] = "zz" }, reasons: []string{rPCRs}},
		{name: "pcr value is short", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["1"] = "abcd" }, reasons: []string{rPCRs}},
		{name: "pcr index is not a number", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["x"] = ev.PCRs["0"] }, reasons: []string{rPCRs}},
		{name: "pcr index is negative", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["-1"] = ev.PCRs["0"] }, reasons: []string{rPCRs}},
		{name: "pcr index has a plus sign", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["+0"] = ev.PCRs["0"]; delete(ev.PCRs, "0") }, reasons: []string{rPCRs}},
		{name: "pcr index spelled twice", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["00"] = ev.PCRs["0"] }, reasons: []string{rPCRs}},
		{name: "extra pcr", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.PCRs["7"] = ev.PCRs["0"] }, reasons: []string{rQuoteSig, rReplay}},
		{name: "missing pcr", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { delete(ev.PCRs, "4") }, reasons: []string{rQuoteSig, rReplay}},
		{name: "pcr value changed", tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
			ev.PCRs["0"] = hex.EncodeToString(make([]byte, 32))
		}, reasons: []string{rQuoteSig, rReplay}},
		{name: "short event digest", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.EventLog[0].SHA256 = "abcd" }, reasons: []string{rReplay, rFirmwareRef}},
		{name: "gpu fields empty", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[0] = GPUReport{Index: 0} }, reasons: []string{
			"gpu 0: device certificate invalid", "gpu 0: report signature invalid",
			"gpu 0: vbios (empty) not allowed", "gpu 0: confidential computing mode off",
		}},
		{name: "short device key", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[1].DevicePub = b64(make([]byte, 5)) }, reasons: []string{
			"gpu 1: device certificate invalid", "gpu 1: report signature invalid",
		}},
		{
			name:    "reference without gpu section",
			tweak:   func(_ *Evidence, _ *Expected, ref *Reference) { ref.GPU = RefGPU{} },
			reasons: referenceWithoutGPUReasons(),
		},
	})
}

func referenceWithoutGPUReasons() []string {
	reasons := []string{rVendorRoot}
	reasons = append(reasons, gpuReasons("device certificate invalid", allGPUs()...)...)
	return append(reasons, gpuReasons("vbios "+testVBIOS+" not allowed", allGPUs()...)...)
}

func TestMeasurementsDigest(t *testing.T) {
	r := newRig(t)
	tampered := r.evidence(withTampered(ComponentKernel))
	pcrs, err := ReplayPCRs(tampered.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(ev *Evidence)
		want string
	}{
		{"digest of the quoted pcrs", func(*Evidence) {}, MeasurementsDigest(pcrs)},
		{"empty when pcrs are malformed", func(ev *Evidence) { ev.PCRs["0"] = "zz" }, ""},
		{"empty when pcrs are absent", func(ev *Evidence) { ev.PCRs = nil }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := r.evidence(withTampered(ComponentKernel))
			c.edit(&ev)
			if got := Verify(ev, r.expected(), r.reference()).MeasurementsDigest; got != c.want {
				t.Fatalf("MeasurementsDigest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestVerifyDoesNotMutateInputs(t *testing.T) {
	r := newRig(t)
	ev := r.evidence(withTampered(ComponentBMC), withoutGPU(2))
	ref := r.reference()
	before, err := json.Marshal([]any{ev, ref})
	if err != nil {
		t.Fatal(err)
	}
	Verify(ev, r.expected(), ref)
	after, err := json.Marshal([]any{ev, ref})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Verify modified its inputs")
	}
}

func TestReferenceVersionIsEchoed(t *testing.T) {
	r := newRig(t)
	ref := r.reference()
	ref.Version = "2027-01-01.7"
	if got := Verify(r.evidence(), r.expected(), ref).ReferenceVersion; got != ref.Version {
		t.Fatalf("ReferenceVersion = %q, want %q", got, ref.Version)
	}
}

func TestExpectedValuesDriveVerification(t *testing.T) {
	r := newRig(t)
	otherAK := testKey("ak-node-002")
	ev := r.evidence(withNode("node-002"), withNonce(testOldNonce), withAK(otherAK))
	want := Expected{NodeID: "node-002", Nonce: testOldNonce, AKPub: pubOf(otherAK)}
	assertReasons(t, Verify(ev, want, r.reference()))
}
