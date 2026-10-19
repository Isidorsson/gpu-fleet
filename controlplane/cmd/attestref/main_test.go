package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
)

const (
	committedSeed = "42"
	committedPath = "../../../deploy/attestation/reference.json"
)

func runAttestref(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return out.String()
}

// The committed file must be exactly what the generator prints, so changing
// the simulator's measurements or keys without regenerating fails CI.
func TestCommittedReferenceMatchesGenerator(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(committedPath))
	if err != nil {
		t.Fatalf("read committed reference values: %v", err)
	}
	var committed attest.Reference
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatalf("committed reference values are not valid: %v", err)
	}

	got := runAttestref(t, "--seed", committedSeed, "--version", committed.Version)

	// git on Windows may check the file out with CRLF.
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got != want {
		t.Fatalf("deploy/attestation/reference.json is stale; regenerate with\n"+
			"  go run ./cmd/attestref --seed %s --version %s\n\ngenerated:\n%s", committedSeed, committed.Version, got)
	}
}

func TestOutputIsIndentedJSONWithTrailingNewline(t *testing.T) {
	got := runAttestref(t, "--seed", "7", "--version", "v1")

	if !strings.HasSuffix(got, "}\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("output must end with exactly one newline: %q", got[max(0, len(got)-5):])
	}
	if !strings.HasPrefix(got, "{\n  \"version\": \"v1\",\n") {
		t.Fatalf("output is not two-space indented JSON:\n%s", got)
	}
	var ref attest.Reference
	if err := json.Unmarshal([]byte(got), &ref); err != nil {
		t.Fatalf("output is not valid reference values: %v", err)
	}
}

func TestVendorRootDependsOnSeed(t *testing.T) {
	a := runAttestref(t, "--seed", "1", "--version", "v1")
	b := runAttestref(t, "--seed", "2", "--version", "v1")
	if a == b {
		t.Fatal("different seeds produced identical reference values")
	}
	if again := runAttestref(t, "--seed", "1", "--version", "v1"); again != a {
		t.Fatal("same seed produced different reference values")
	}
}

func TestRequiredFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"missing seed", []string{"--version", "v1"}},
		{"missing version", []string{"--seed", "42"}},
		{"empty version", []string{"--seed", "42", "--version", ""}},
		{"bad seed", []string{"--seed", "forty-two", "--version", "v1"}},
		{"negative seed", []string{"--seed", "-1", "--version", "v1"}},
		{"unknown flag", []string{"--seed", "42", "--version", "v1", "--nodes", "3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(tc.args, &out); err == nil {
				t.Fatalf("run %v succeeded, want an error", tc.args)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote output despite the error: %q", out.String())
			}
		})
	}
}

func TestSeedZeroIsAccepted(t *testing.T) {
	runAttestref(t, "--seed", "0", "--version", "v1")
}
