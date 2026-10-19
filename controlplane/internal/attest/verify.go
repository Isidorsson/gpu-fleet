package attest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

const (
	reasonNonceMismatch  = "nonce mismatch"
	reasonNodeIDMismatch = "node_id mismatch"
	reasonAKInvalid      = "enrolled attestation key invalid"
	reasonPCRsMalformed  = "quoted pcrs malformed"
	reasonQuoteSigBad    = "quote signature invalid"
	reasonLogReplay      = "event log does not replay to quoted pcrs"
	reasonLogShape       = "event log missing or out of order"
)

// Verify checks evidence against what the verifier knows independently of it:
// the expected node ID and nonce, the enrolled attestation key and the
// reference values. It is pure and never panics, whatever the input. Every
// check runs, so one tampered component never hides another.
//
// Result.Verdict is VerdictPass only when there are no reasons. Reasons are
// sorted and deduplicated. The complete list of reason strings:
//
//	nonce mismatch
//	node_id mismatch
//	enrolled attestation key invalid
//	quoted pcrs malformed
//	quote signature invalid
//	event log does not replay to quoted pcrs
//	event log missing or out of order
//	firmware digest not in reference values
//	bmc digest not in reference values
//	bootloader digest not in reference values
//	kernel digest not in reference values
//	expected 8 gpus, got N
//	reference vendor root invalid
//	gpu N: index out of range
//	gpu N: duplicate index
//	gpu N: device certificate invalid
//	gpu N: report signature invalid
//	gpu N: vbios V not allowed
//	gpu N: confidential computing mode off
//
// "quoted pcrs malformed" means the pcrs map has a non-canonical index, or a
// value that is not 32 hex-encoded bytes; the quote signature and the replay
// comparison are then skipped. "enrolled attestation key invalid" means
// Expected.AKPub is not a 32 byte key, so the quote signature is not checked.
// "reference vendor root invalid" means Reference.GPU.VendorRootPub is not a
// base64 encoded 32 byte key, so every device certificate also fails.
// "digest not in reference values" compares digest bytes only; the event's
// name is informational. In "gpu N: vbios V not allowed", V is the reported
// VBIOS with characters outside [0-9A-Za-z._-] replaced by '?', cut to 64
// characters, or "(empty)". "confidential computing mode off" is reported for
// any cc_mode other than "on" when Reference.GPU.RequireCCMode is set. N is
// the index the GPU claims for itself.
//
// The quote signature is checked over QuoteMessage(want.NodeID, want.Nonce,
// quoted pcrs), and the GPU signatures over messages built from want.NodeID
// and want.Nonce, so evidence cannot choose what its signatures cover.
// Result.MeasurementsDigest is empty when the quoted pcrs are absent or
// malformed.
func Verify(ev Evidence, want Expected, ref Reference) Result {
	rs := reasonSet{}
	quoted := parseQuotedPCRs(ev.PCRs)
	if !quoted.ok {
		rs.add(reasonPCRsMalformed)
	}
	checkIdentity(rs, ev, want)
	checkQuote(rs, ev.QuoteSig, want, quoted)
	checkEventLog(rs, ev.EventLog, quoted)
	checkEventDigests(rs, ev.EventLog, ref)
	checkGPUs(rs, ev.GPUs, want, ref)
	return newResult(rs, want, ref, quoted)
}

type reasonSet map[string]struct{}

func (s reasonSet) add(reason string) { s[reason] = struct{}{} }

func (s reasonSet) sorted() []string {
	out := slices.AppendSeq(make([]string, 0, len(s)), maps.Keys(s))
	slices.Sort(out)
	return out
}

type quotedPCRs struct {
	values map[int][]byte
	ok     bool
}

// parseQuotedPCRs is stricter than ParsePCRs: keys must be canonical decimal
// so two spellings of one index cannot make the result depend on map order.
func parseQuotedPCRs(raw map[string]string) quotedPCRs {
	values, err := ParsePCRs(raw)
	if err != nil {
		return quotedPCRs{}
	}
	for key := range raw {
		idx, err := strconv.Atoi(key)
		if err != nil || strconv.Itoa(idx) != key {
			return quotedPCRs{}
		}
	}
	return quotedPCRs{values: values, ok: true}
}

func checkIdentity(rs reasonSet, ev Evidence, want Expected) {
	if ev.NodeID != want.NodeID {
		rs.add(reasonNodeIDMismatch)
	}
	if ev.Nonce != want.Nonce {
		rs.add(reasonNonceMismatch)
	}
}

func checkQuote(rs reasonSet, quoteSig string, want Expected, quoted quotedPCRs) {
	if len(want.AKPub) != ed25519.PublicKeySize {
		rs.add(reasonAKInvalid)
		return
	}
	if !quoted.ok {
		return
	}
	sig, sigOK := decodeBase64(quoteSig, ed25519.SignatureSize)
	msg := QuoteMessage(want.NodeID, want.Nonce, quoted.values)
	if !sigOK || !ed25519.Verify(want.AKPub, msg, sig) {
		rs.add(reasonQuoteSigBad)
	}
}

func checkEventLog(rs reasonSet, log []Event, quoted quotedPCRs) {
	if !hasRequiredEvents(log) {
		rs.add(reasonLogShape)
	}
	if quoted.ok && !replaysTo(log, quoted.values) {
		rs.add(reasonLogReplay)
	}
}

func hasRequiredEvents(log []Event) bool {
	if len(log) != len(Measured) {
		return false
	}
	for i, m := range Measured {
		if log[i].PCR != m.PCR || log[i].Component != m.Component {
			return false
		}
	}
	return true
}

func replaysTo(log []Event, quoted map[int][]byte) bool {
	replayed, err := ReplayPCRs(log)
	return err == nil && maps.EqualFunc(replayed, quoted, bytes.Equal)
}

func checkEventDigests(rs reasonSet, log []Event, ref Reference) {
	for _, ev := range log {
		if !isMeasured(ev.Component) {
			continue
		}
		if !referenceAllows(ref.Components[ev.Component], ev.SHA256) {
			rs.add(fmt.Sprintf("%s digest not in reference values", ev.Component))
		}
	}
}

func isMeasured(c Component) bool {
	for _, m := range Measured {
		if m.Component == c {
			return true
		}
	}
	return false
}

func referenceAllows(allowed []RefMeasure, digestHex string) bool {
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(allowed, func(m RefMeasure) bool {
		want, err := hex.DecodeString(m.SHA256)
		return err == nil && bytes.Equal(want, digest)
	})
}

func newResult(rs reasonSet, want Expected, ref Reference, quoted quotedPCRs) Result {
	reasons := rs.sorted()
	verdict := VerdictPass
	if len(reasons) > 0 {
		verdict = VerdictFail
	}
	digest := ""
	if quoted.ok && len(quoted.values) > 0 {
		digest = MeasurementsDigest(quoted.values)
	}
	return Result{
		NodeID:             want.NodeID,
		Verdict:            verdict,
		Reasons:            reasons,
		Nonce:              want.Nonce,
		MeasurementsDigest: digest,
		ReferenceVersion:   ref.Version,
	}
}

func decodeBase64(s string, size int) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	return b, err == nil && len(b) == size
}
