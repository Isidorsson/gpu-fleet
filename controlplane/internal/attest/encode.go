package attest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The functions below produce the exact bytes in docs/attestation.md
// "Signed messages". Attester, verifier and control plane all use them.

func QuoteMessage(nodeID, nonce string, pcrs map[int][]byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "gpu-fleet-quote-v1\n%s\n%s\n", nodeID, nonce)
	for _, idx := range sortedPCRs(pcrs) {
		fmt.Fprintf(&b, "%d:%s\n", idx, hex.EncodeToString(pcrs[idx]))
	}
	return []byte(b.String())
}

func GPUCertMessage(nodeID string, index int, devicePubB64 string) []byte {
	return fmt.Appendf(nil, "gpu-fleet-gpu-cert-v1\n%s\n%d\n%s\n", nodeID, index, devicePubB64)
}

func GPUReportMessage(nodeID, nonce string, index int, vbios, ccMode string) []byte {
	return fmt.Appendf(nil, "gpu-fleet-gpu-report-v1\n%s\n%s\n%d\n%s\n%s\n", nodeID, nonce, index, vbios, ccMode)
}

func ResultMessage(r Result) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "gpu-fleet-attestation-result-v1\n%s\n%s\n%s\n%s\n%s\n%s\n",
		r.NodeID, r.Verdict, r.Nonce, r.MeasurementsDigest, r.ReferenceVersion,
		r.AttestedAt.UTC().Format(time.RFC3339))
	for _, reason := range r.Reasons {
		b.WriteString(reason + "\n")
	}
	return []byte(b.String())
}

// Extend returns sha256(pcr || digest).
func Extend(pcr, digest []byte) []byte {
	sum := sha256.Sum256(append(slices.Clone(pcr), digest...))
	return sum[:]
}

// ReplayPCRs extends zeroed PCRs with the log's digests in order.
func ReplayPCRs(log []Event) (map[int][]byte, error) {
	pcrs := map[int][]byte{}
	for i, ev := range log {
		digest, err := hex.DecodeString(ev.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("event %d: sha256 is not 32 hex-encoded bytes", i)
		}
		cur, ok := pcrs[ev.PCR]
		if !ok {
			cur = make([]byte, sha256.Size)
		}
		pcrs[ev.PCR] = Extend(cur, digest)
	}
	return pcrs, nil
}

// MeasurementsDigest is sha256 over the PCR values in ascending index order.
func MeasurementsDigest(pcrs map[int][]byte) string {
	h := sha256.New()
	for _, idx := range sortedPCRs(pcrs) {
		h.Write(pcrs[idx])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ComponentDigest(name string) []byte {
	sum := sha256.Sum256([]byte("gpu-fleet/component/" + name))
	return sum[:]
}

func TamperedDigest(c Component) []byte {
	sum := sha256.Sum256([]byte("gpu-fleet/tampered/" + string(c)))
	return sum[:]
}

// ParsePCRs converts the evidence's {"0": hex} map.
func ParsePCRs(raw map[string]string) (map[int][]byte, error) {
	out := make(map[int][]byte, len(raw))
	for k, v := range raw {
		idx, err := strconv.Atoi(k)
		if err != nil || idx < 0 || strconv.Itoa(idx) != k {
			return nil, fmt.Errorf("pcr index %q is not a canonical non-negative integer", k)
		}
		val, err := hex.DecodeString(v)
		if err != nil || len(val) != sha256.Size {
			return nil, fmt.Errorf("pcr %d is not 32 hex-encoded bytes", idx)
		}
		out[idx] = val
	}
	return out, nil
}

func sortedPCRs(pcrs map[int][]byte) []int {
	idx := make([]int, 0, len(pcrs))
	for i := range pcrs {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	return idx
}
