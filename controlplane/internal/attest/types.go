// Package attest holds the attestation evidence format, the exact signed
// message encodings and the pure verifier. See docs/attestation.md.
package attest

import (
	"crypto/ed25519"
	"time"
)

type Component string

const (
	ComponentFirmware   Component = "firmware"
	ComponentBMC        Component = "bmc"
	ComponentBootloader Component = "bootloader"
	ComponentKernel     Component = "kernel"
)

// Measured lists the measured components in event log order with their PCR.
var Measured = []struct {
	PCR       int
	Component Component
}{
	{0, ComponentFirmware},
	{1, ComponentBMC},
	{4, ComponentBootloader},
	{4, ComponentKernel},
}

const (
	GPUsPerNode = 8
	CCModeOn    = "on"
	CCModeOff   = "off"
)

// Evidence is the wire format of GET /agent/{id}/attestation.
type Evidence struct {
	NodeID   string            `json:"node_id"`
	Nonce    string            `json:"nonce"`
	PCRs     map[string]string `json:"pcrs"`
	EventLog []Event           `json:"event_log"`
	QuoteSig string            `json:"quote_sig"`
	GPUs     []GPUReport       `json:"gpus"`
}

type Event struct {
	PCR       int       `json:"pcr"`
	Component Component `json:"component"`
	Name      string    `json:"name"`
	SHA256    string    `json:"sha256"`
}

type GPUReport struct {
	Index         int    `json:"index"`
	VBIOS         string `json:"vbios"`
	CCMode        string `json:"cc_mode"`
	DevicePub     string `json:"device_pub"`
	DeviceCertSig string `json:"device_cert_sig"`
	ReportSig     string `json:"report_sig"`
}

// Reference is the parsed deploy/attestation/reference.json.
type Reference struct {
	Version    string                     `json:"version"`
	Components map[Component][]RefMeasure `json:"components"`
	GPU        RefGPU                     `json:"gpu"`
}

type RefMeasure struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type RefGPU struct {
	VendorRootPub string   `json:"vendor_root_pub"`
	VBIOS         []string `json:"vbios"`
	RequireCCMode bool     `json:"require_cc_mode"`
}

// Expected is what the verifier holds independently of the evidence.
type Expected struct {
	NodeID string
	Nonce  string
	AKPub  ed25519.PublicKey
}

type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictFail Verdict = "fail"
)

// Result is a verification outcome. Verify fills everything except
// AttestedAt and Signature, which the control plane adds when it signs.
type Result struct {
	NodeID             string
	Verdict            Verdict
	Reasons            []string
	Nonce              string
	MeasurementsDigest string
	ReferenceVersion   string
	AttestedAt         time.Time
	Signature          []byte
}
