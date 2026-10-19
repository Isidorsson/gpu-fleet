package attest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode"
)

const sha256HexLen = 2 * sha256.Size

// ErrInvalidReference is wrapped by every ParseReference and LoadReference
// validation error.
var ErrInvalidReference = errors.New("invalid reference values")

// LoadReference reads and parses a reference values file.
func LoadReference(path string) (Reference, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Reference{}, fmt.Errorf("read reference values: %w", err)
	}
	ref, err := ParseReference(data)
	if err != nil {
		return Reference{}, fmt.Errorf("parse reference values %s: %w", path, err)
	}
	return ref, nil
}

// ParseReference decodes reference values strictly: unknown fields, trailing
// data and incomplete or malformed values are all errors.
func ParseReference(data []byte) (Reference, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ref Reference
	if err := dec.Decode(&ref); err != nil {
		return Reference{}, fmt.Errorf("%w: %w", ErrInvalidReference, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Reference{}, fmt.Errorf("%w: unexpected data after the JSON document", ErrInvalidReference)
	}
	if err := errors.Join(validateReference(ref), requireCCModeKey(data)); err != nil {
		return Reference{}, fmt.Errorf("%w: %w", ErrInvalidReference, err)
	}
	return ref, nil
}

// requireCCModeKey fails closed: a policy that omits require_cc_mode must not
// silently decode as false and disable the confidential computing check.
func requireCCModeKey(data []byte) error {
	var presence struct {
		GPU struct {
			RequireCCMode *bool `json:"require_cc_mode"`
		} `json:"gpu"`
	}
	if err := json.Unmarshal(data, &presence); err != nil || presence.GPU.RequireCCMode == nil {
		return errors.New("gpu.require_cc_mode: must be set explicitly")
	}
	return nil
}

func validateReference(ref Reference) error {
	errs := []error{validateVersion(ref.Version)}
	errs = append(errs, validateComponents(ref.Components)...)
	errs = append(errs, validateGPU(ref.GPU)...)
	return errors.Join(errs...)
}

func validateVersion(version string) error {
	switch {
	case strings.TrimSpace(version) == "":
		return errors.New("version is empty")
	case hasControl(version):
		return errors.New("version contains control characters")
	}
	return nil
}

func validateComponents(components map[Component][]RefMeasure) []error {
	var errs []error
	for _, m := range Measured {
		errs = append(errs, validateMeasures(m.Component, components[m.Component])...)
	}
	for _, c := range slices.Sorted(maps.Keys(components)) {
		if !isMeasured(c) {
			errs = append(errs, fmt.Errorf("components.%s: unknown component", c))
		}
	}
	return errs
}

func validateMeasures(c Component, entries []RefMeasure) []error {
	if len(entries) == 0 {
		return []error{fmt.Errorf("components.%s: no entries", c)}
	}
	var errs []error
	for i, e := range entries {
		if !isLowerHexSHA256(e.SHA256) {
			errs = append(errs, fmt.Errorf("components.%s[%d].sha256: want %d lowercase hex characters", c, i, sha256HexLen))
		}
	}
	return errs
}

func validateGPU(gpu RefGPU) []error {
	var errs []error
	if _, ok := decodeBase64(gpu.VendorRootPub, ed25519.PublicKeySize); !ok {
		errs = append(errs, fmt.Errorf("gpu.vendor_root_pub: want base64 of %d bytes", ed25519.PublicKeySize))
	}
	if len(gpu.VBIOS) == 0 {
		errs = append(errs, errors.New("gpu.vbios: at least one entry is required"))
	}
	for i, v := range gpu.VBIOS {
		if strings.TrimSpace(v) == "" || hasControl(v) {
			errs = append(errs, fmt.Errorf("gpu.vbios[%d]: empty or contains control characters", i))
		}
	}
	return errs
}

func isLowerHexSHA256(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
