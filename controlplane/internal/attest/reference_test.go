package attest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func marshalReference(t *testing.T, mutate func(*Reference)) []byte {
	t.Helper()
	ref := newRig(t).reference()
	if mutate != nil {
		mutate(&ref)
	}
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseReferenceRoundTrip(t *testing.T) {
	want := newRig(t).reference()
	got, err := ParseReference(marshalReference(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseReferenceAcceptsContractExample(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	root := b64(make([]byte, 32))
	doc := `{
	  "version": "2026-10-19.1",
	  "components": {
	    "firmware":   [{"name": "bios-2.4.1", "sha256": "` + digest + `"}],
	    "bmc":        [{"name": "bmc-1.9.3", "sha256": "` + digest + `"}],
	    "bootloader": [{"name": "grub-2.12", "sha256": "` + digest + `"}],
	    "kernel":     [{"name": "inspect-ramdisk-6.8.0", "sha256": "` + digest + `"}]
	  },
	  "gpu": {"vendor_root_pub": "` + root + `", "vbios": ["96.00.9F.00.01"], "require_cc_mode": true}
	}` + "\n"
	ref, err := ParseReference([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Version != "2026-10-19.1" || !ref.GPU.RequireCCMode || len(ref.Components) != 4 {
		t.Fatalf("parsed wrong: %+v", ref)
	}
}

func TestParseReferenceAcceptsSeveralEntries(t *testing.T) {
	data := marshalReference(t, func(ref *Reference) {
		ref.Components[ComponentFirmware] = append(ref.Components[ComponentFirmware], refMeasure("bios-2.5.0"))
		ref.GPU.VBIOS = append(ref.GPU.VBIOS, "96.00.A0.00.02")
		ref.GPU.RequireCCMode = false
	})
	ref, err := ParseReference(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(ref.Components[ComponentFirmware]) != 2 || len(ref.GPU.VBIOS) != 2 || ref.GPU.RequireCCMode {
		t.Fatalf("parsed wrong: %+v", ref)
	}
}

func TestParseReferenceRejects(t *testing.T) {
	valid := string(marshalReference(t, nil))
	cases := []struct {
		name    string
		data    string
		mutate  func(*Reference)
		wantErr string
	}{
		{name: "empty input", data: "", wantErr: "EOF"},
		{name: "not json", data: "{", wantErr: "unexpected EOF"},
		{name: "unknown field", data: strings.Replace(valid, `{"version"`, `{"extra":1,"version"`, 1), wantErr: "unknown field"},
		{name: "unknown nested field", data: strings.Replace(valid, `"require_cc_mode"`, `"require_cc":true,"require_cc_mode"`, 1), wantErr: "unknown field"},
		{name: "second document", data: valid + valid, wantErr: "after the JSON document"},
		{name: "trailing garbage", data: valid + "x", wantErr: "after the JSON document"},
		{name: "empty version", mutate: func(r *Reference) { r.Version = "" }, wantErr: "version is empty"},
		{name: "blank version", mutate: func(r *Reference) { r.Version = "  " }, wantErr: "version is empty"},
		{name: "version with newline", mutate: func(r *Reference) { r.Version = "v1\nfail" }, wantErr: "control characters"},
		{name: "missing component", mutate: func(r *Reference) { delete(r.Components, ComponentBMC) }, wantErr: "components.bmc: no entries"},
		{name: "no components", mutate: func(r *Reference) { r.Components = nil }, wantErr: "components.firmware: no entries"},
		{name: "empty component list", mutate: func(r *Reference) { r.Components[ComponentKernel] = nil }, wantErr: "components.kernel: no entries"},
		{name: "unknown component", mutate: func(r *Reference) { r.Components["firmwre"] = r.Components[ComponentFirmware] }, wantErr: "components.firmwre: unknown component"},
		{name: "uppercase sha256", mutate: func(r *Reference) {
			r.Components[ComponentFirmware][0].SHA256 = strings.ToUpper(r.Components[ComponentFirmware][0].SHA256)
		}, wantErr: "components.firmware[0].sha256"},
		{name: "short sha256", mutate: func(r *Reference) { r.Components[ComponentBMC][0].SHA256 = "abcd" }, wantErr: "components.bmc[0].sha256"},
		{name: "non-hex sha256", mutate: func(r *Reference) { r.Components[ComponentBootloader][0].SHA256 = strings.Repeat("g", 64) }, wantErr: "components.bootloader[0].sha256"},
		{name: "bad second entry", mutate: func(r *Reference) {
			r.Components[ComponentKernel] = append(r.Components[ComponentKernel], RefMeasure{Name: "x", SHA256: ""})
		}, wantErr: "components.kernel[1].sha256"},
		{name: "vendor root not base64", mutate: func(r *Reference) { r.GPU.VendorRootPub = "***" }, wantErr: "gpu.vendor_root_pub"},
		{name: "vendor root wrong size", mutate: func(r *Reference) { r.GPU.VendorRootPub = b64(make([]byte, 31)) }, wantErr: "gpu.vendor_root_pub"},
		{name: "vendor root missing", mutate: func(r *Reference) { r.GPU.VendorRootPub = "" }, wantErr: "gpu.vendor_root_pub"},
		{name: "no vbios", mutate: func(r *Reference) { r.GPU.VBIOS = nil }, wantErr: "gpu.vbios: at least one entry"},
		{name: "empty vbios entry", mutate: func(r *Reference) { r.GPU.VBIOS = []string{testVBIOS, ""} }, wantErr: "gpu.vbios[1]"},
		{name: "vbios with newline", mutate: func(r *Reference) { r.GPU.VBIOS = []string{"96\n00"} }, wantErr: "gpu.vbios[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(c.data)
			if c.mutate != nil {
				data = marshalReference(t, c.mutate)
			}
			_, err := ParseReference(data)
			if err == nil {
				t.Fatal("want error")
			}
			if !errors.Is(err, ErrInvalidReference) {
				t.Errorf("error %v does not wrap ErrInvalidReference", err)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestParseReferenceReportsEveryProblem(t *testing.T) {
	data := marshalReference(t, func(r *Reference) {
		r.Version = ""
		delete(r.Components, ComponentBMC)
		r.GPU.VBIOS = nil
	})
	_, err := ParseReference(data)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"version is empty", "components.bmc: no entries", "gpu.vbios: at least one entry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestParseReferenceReturnsZeroValueOnError(t *testing.T) {
	ref, err := ParseReference([]byte(`{"version":"v1"}`))
	if err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(ref, Reference{}) {
		t.Fatalf("want zero Reference on error, got %+v", ref)
	}
}

func TestLoadReference(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "reference.json")
	if err := os.WriteFile(good, marshalReference(t, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := LoadReference(good)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Version != testRefVer {
		t.Fatalf("Version = %q", ref.Version)
	}

	_, err = LoadReference(filepath.Join(dir, "missing.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: error %v does not wrap fs.ErrNotExist", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"version":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadReference(bad)
	if !errors.Is(err, ErrInvalidReference) || !strings.Contains(err.Error(), bad) {
		t.Fatalf("bad file: error %v should wrap ErrInvalidReference and name the path", err)
	}
}

func TestParseReferenceRequiresExplicitCCMode(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(marshalReference(t, nil), &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["gpu"].(map[string]any), "require_cc_mode")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseReference(data)
	if !errors.Is(err, ErrInvalidReference) || !strings.Contains(err.Error(), "require_cc_mode") {
		t.Fatalf("err = %v, want a require_cc_mode error", err)
	}
}
