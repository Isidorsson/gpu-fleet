package attest

import (
	"bytes"
	"testing"
)

func TestGPUCountAndIndices(t *testing.T) {
	runScenarios(t, []scenario{
		{name: "seven gpus", opts: []option{withoutGPU(7)}, reasons: []string{"expected 8 gpus, got 7"}},
		{name: "no gpus", tweak: func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs = nil }, reasons: []string{"expected 8 gpus, got 0"}},
		{
			name:    "nine gpus",
			opts:    []option{withExtraGPU(8)},
			reasons: []string{"expected 8 gpus, got 9", "gpu 8: index out of range"},
		},
		{name: "duplicate index", opts: []option{withGPUIndex(5, 3)}, reasons: gpuReasons("duplicate index", 3)},
		{name: "negative index", opts: []option{withGPUIndex(5, -1)}, reasons: gpuReasons("index out of range", -1)},
		{name: "index above range", opts: []option{withGPUIndex(7, 8)}, reasons: gpuReasons("index out of range", 8)},
		{
			name:    "relabelled index breaks its signatures",
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[3].Index = 4 },
			reasons: append(gpuReasons("device certificate invalid", 4), "gpu 4: report signature invalid", "gpu 4: duplicate index"),
		},
	})
}

func TestGPUAttributes(t *testing.T) {
	attacker := testKey("attacker")
	runScenarios(t, []scenario{
		{name: "tampered vbios", opts: []option{withVBIOS(3, "99.99.99.99.99")}, reasons: []string{"gpu 3: vbios 99.99.99.99.99 not allowed"}},
		{name: "empty vbios", opts: []option{withVBIOS(0, "")}, reasons: []string{"gpu 0: vbios (empty) not allowed"}},
		{
			name:    "vbios text cannot inject a line",
			opts:    []option{withVBIOS(2, testVBIOS+"\nverdict pass")},
			reasons: []string{"gpu 2: vbios 96.00.9F.00.01?verdict?pass not allowed"},
		},
		{
			name:    "vbios text is cut",
			opts:    []option{withVBIOS(2, string(bytes.Repeat([]byte("A"), 300)))},
			reasons: []string{"gpu 2: vbios " + string(bytes.Repeat([]byte("A"), maxReasonValueRunes)) + " not allowed"},
		},
		{name: "cc mode off", opts: []option{withCCMode(3, CCModeOff)}, reasons: []string{"gpu 3: confidential computing mode off"}},
		{name: "cc mode unknown value", opts: []option{withCCMode(3, "devtools")}, reasons: []string{"gpu 3: confidential computing mode off"}},
		{
			name:  "cc mode off but not required",
			opts:  []option{withCCMode(3, CCModeOff)},
			tweak: func(_ *Evidence, _ *Expected, ref *Reference) { ref.GPU.RequireCCMode = false },
		},
		{
			name:    "cc mode flipped on the wire",
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[3].CCMode = CCModeOff },
			reasons: []string{"gpu 3: confidential computing mode off", "gpu 3: report signature invalid"},
		},
		{
			name:    "vbios edited on the wire",
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[3].VBIOS = "99.99.99.99.99" },
			reasons: []string{"gpu 3: vbios 99.99.99.99.99 not allowed", "gpu 3: report signature invalid"},
		},
		{
			name: "certificate from another root",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.GPUs[3].DeviceCertSig = signB64(attacker, GPUCertMessage(testNodeID, 3, ev.GPUs[3].DevicePub))
			},
			reasons: gpuReasons("device certificate invalid", 3),
		},
		{
			name: "device key swapped for the attacker's",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				g := &ev.GPUs[3]
				g.DevicePub = b64(pubOf(attacker))
				g.ReportSig = signB64(attacker, GPUReportMessage(testNodeID, testNonce, 3, g.VBIOS, g.CCMode))
			},
			reasons: gpuReasons("device certificate invalid", 3),
		},
		{
			name: "report signature flipped",
			tweak: func(ev *Evidence, _ *Expected, _ *Reference) {
				ev.GPUs[3].ReportSig = corruptB64(t, ev.GPUs[3].ReportSig)
			},
			reasons: gpuReasons("report signature invalid", 3),
		},
		{
			name:    "device key is not base64",
			tweak:   func(ev *Evidence, _ *Expected, _ *Reference) { ev.GPUs[3].DevicePub = "???" },
			reasons: append(gpuReasons("device certificate invalid", 3), "gpu 3: report signature invalid"),
		},
		{
			name:    "vendor root is another key",
			tweak:   func(_ *Evidence, _ *Expected, ref *Reference) { ref.GPU.VendorRootPub = b64(pubOf(attacker)) },
			reasons: gpuReasons("device certificate invalid", allGPUs()...),
		},
		{
			name:    "vendor root in the reference is unusable",
			tweak:   func(_ *Evidence, _ *Expected, ref *Reference) { ref.GPU.VendorRootPub = "AAAA" },
			reasons: append(gpuReasons("device certificate invalid", allGPUs()...), rVendorRoot),
		},
	})
}
