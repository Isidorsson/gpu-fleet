package attest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestQuoteMessageOrdersPCRs(t *testing.T) {
	pcrs := map[int][]byte{4: {0x04}, 0: {0x00}, 1: {0x01}}
	want := "gpu-fleet-quote-v1\nnode-001\nab\n0:00\n1:01\n4:04\n"
	if got := string(QuoteMessage("node-001", "ab", pcrs)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGPUMessages(t *testing.T) {
	if got, want := string(GPUCertMessage("n", 3, "PUB=")), "gpu-fleet-gpu-cert-v1\nn\n3\nPUB=\n"; got != want {
		t.Errorf("cert = %q, want %q", got, want)
	}
	if got, want := string(GPUReportMessage("n", "ab", 3, "96.00", "on")), "gpu-fleet-gpu-report-v1\nn\nab\n3\n96.00\non\n"; got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
}

func TestResultMessage(t *testing.T) {
	r := Result{
		NodeID: "n", Verdict: VerdictFail, Nonce: "ab", MeasurementsDigest: "cd",
		ReferenceVersion: "v1", AttestedAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.FixedZone("CEST", 7200)),
		Reasons: []string{"a", "b"}, Signature: []byte("ignored"),
	}
	want := "gpu-fleet-attestation-result-v1\nn\nfail\nab\ncd\nv1\n2026-10-19T10:00:00Z\na\nb\n"
	if got := string(ResultMessage(r)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReplayMatchesManualExtend(t *testing.T) {
	d1, d2 := ComponentDigest("grub-2.12"), ComponentDigest("inspect-ramdisk-6.8.0")
	log := []Event{
		{PCR: 4, SHA256: hex.EncodeToString(d1)},
		{PCR: 4, SHA256: hex.EncodeToString(d2)},
	}
	got, err := ReplayPCRs(log)
	if err != nil {
		t.Fatal(err)
	}
	want := Extend(Extend(make([]byte, sha256.Size), d1), d2)
	if !bytes.Equal(got[4], want) || len(got) != 1 {
		t.Fatalf("pcrs = %x, want only pcr 4 = %x", got, want)
	}
}

func TestReplayRejectsBadDigest(t *testing.T) {
	if _, err := ReplayPCRs([]Event{{PCR: 0, SHA256: "zz"}}); err == nil {
		t.Fatal("want error for non-hex digest")
	}
}

func TestExtendDoesNotMutateInput(t *testing.T) {
	pcr := make([]byte, sha256.Size, 64)
	Extend(pcr, ComponentDigest("x"))
	if !bytes.Equal(pcr, make([]byte, sha256.Size)) {
		t.Fatal("Extend mutated its input")
	}
}

func TestParsePCRs(t *testing.T) {
	ok := hex.EncodeToString(make([]byte, sha256.Size))
	if _, err := ParsePCRs(map[string]string{"0": ok, "4": ok}); err != nil {
		t.Fatalf("valid pcrs: %v", err)
	}
	for _, bad := range []map[string]string{{"x": ok}, {"-1": ok}, {"0": "abcd"}, {"00": ok}, {"+0": ok}} {
		if _, err := ParsePCRs(bad); err == nil {
			t.Errorf("ParsePCRs(%v): want error", bad)
		}
	}
}

func TestDigestsAreDistinct(t *testing.T) {
	if bytes.Equal(ComponentDigest("bootloader"), TamperedDigest(ComponentBootloader)) {
		t.Fatal("tampered digest must differ from any component digest")
	}
}
