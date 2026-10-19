package attest

import (
	"crypto/ed25519"
	"fmt"
	"slices"
)

const (
	reasonVendorRootBad = "reference vendor root invalid"

	maxReasonValueRunes = 64
	emptyReasonValue    = "(empty)"
)

type gpuChecker struct {
	rs   reasonSet
	want Expected
	ref  Reference
	root ed25519.PublicKey
	seen map[int]bool
}

func checkGPUs(rs reasonSet, gpus []GPUReport, want Expected, ref Reference) {
	if len(gpus) != GPUsPerNode {
		rs.add(fmt.Sprintf("expected %d gpus, got %d", GPUsPerNode, len(gpus)))
	}
	c := gpuChecker{rs: rs, want: want, ref: ref, seen: map[int]bool{}}
	if root, ok := decodeBase64(ref.GPU.VendorRootPub, ed25519.PublicKeySize); ok {
		c.root = root
	} else {
		rs.add(reasonVendorRootBad)
	}
	for _, g := range gpus {
		c.check(g)
	}
}

func (c *gpuChecker) check(g GPUReport) {
	devicePub, pubOK := decodeBase64(g.DevicePub, ed25519.PublicKeySize)
	c.checkIndex(g.Index)
	c.checkCert(g, pubOK)
	c.checkReport(g, devicePub, pubOK)
	c.checkVBIOS(g)
	c.checkCCMode(g)
}

func (c *gpuChecker) fail(index int, msg string) {
	c.rs.add(fmt.Sprintf("gpu %d: %s", index, msg))
}

func (c *gpuChecker) checkIndex(index int) {
	switch {
	case index < 0 || index >= GPUsPerNode:
		c.fail(index, "index out of range")
	case c.seen[index]:
		c.fail(index, "duplicate index")
	}
	c.seen[index] = true
}

func (c *gpuChecker) checkCert(g GPUReport, pubOK bool) {
	sig, sigOK := decodeBase64(g.DeviceCertSig, ed25519.SignatureSize)
	if !pubOK || !sigOK || c.root == nil {
		c.fail(g.Index, "device certificate invalid")
		return
	}
	msg := GPUCertMessage(c.want.NodeID, g.Index, g.DevicePub)
	if !ed25519.Verify(c.root, msg, sig) {
		c.fail(g.Index, "device certificate invalid")
	}
}

func (c *gpuChecker) checkReport(g GPUReport, devicePub ed25519.PublicKey, pubOK bool) {
	sig, sigOK := decodeBase64(g.ReportSig, ed25519.SignatureSize)
	if !pubOK || !sigOK {
		c.fail(g.Index, "report signature invalid")
		return
	}
	msg := GPUReportMessage(c.want.NodeID, c.want.Nonce, g.Index, g.VBIOS, g.CCMode)
	if !ed25519.Verify(devicePub, msg, sig) {
		c.fail(g.Index, "report signature invalid")
	}
}

func (c *gpuChecker) checkVBIOS(g GPUReport) {
	if !slices.Contains(c.ref.GPU.VBIOS, g.VBIOS) {
		c.fail(g.Index, fmt.Sprintf("vbios %s not allowed", safeReasonValue(g.VBIOS)))
	}
}

func (c *gpuChecker) checkCCMode(g GPUReport) {
	if c.ref.GPU.RequireCCMode && g.CCMode != CCModeOn {
		c.fail(g.Index, "confidential computing mode off")
	}
}

// safeReasonValue keeps attacker-controlled text out of reasons: reasons end
// up in a newline-delimited signed message and in API responses.
func safeReasonValue(s string) string {
	if s == "" {
		return emptyReasonValue
	}
	out := make([]rune, 0, min(len(s), maxReasonValueRunes))
	for _, r := range s {
		if len(out) == maxReasonValueRunes {
			break
		}
		out = append(out, safeRune(r))
	}
	return string(out)
}

func safeRune(r rune) rune {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '_':
		return r
	}
	return '?'
}
