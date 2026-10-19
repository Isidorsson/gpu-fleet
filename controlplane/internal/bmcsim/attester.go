package bmcsim

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"

	"gpu-fleet/controlplane/internal/attest"
)

const (
	DefaultVBIOS  = "96.00.9F.00.01"
	TamperedVBIOS = "99.99.99.99.99"
	unknownName   = "unknown"
)

var defaultNames = map[attest.Component]string{
	attest.ComponentFirmware:   "bios-2.4.1",
	attest.ComponentBMC:        "bmc-1.9.3",
	attest.ComponentBootloader: "grub-2.12",
	attest.ComponentKernel:     "inspect-ramdisk-6.8.0",
}

// DefaultName is the name the simulator reports for an untampered component.
func DefaultName(c attest.Component) string {
	return defaultNames[c]
}

// attester holds one node's attestation keys. It is immutable after
// construction; what a boot measured comes in as the machine's tamper set.
type attester struct {
	nodeID   string
	ak       ed25519.PrivateKey
	forgedAK ed25519.PrivateKey
	gpus     [attest.GPUsPerNode]gpuIdentity
}

type gpuIdentity struct {
	key     ed25519.PrivateKey
	pubB64  string
	certSig string
}

func newAttester(seed uint64, nodeID string, vendorRoot ed25519.PrivateKey) *attester {
	a := &attester{
		nodeID:   nodeID,
		ak:       akKey(seed, nodeID),
		forgedAK: forgedAKKey(seed, nodeID),
	}
	for i := range a.gpus {
		key := gpuKey(seed, nodeID, i)
		pub := b64(publicOf(key))
		a.gpus[i] = gpuIdentity{
			key:     key,
			pubB64:  pub,
			certSig: b64(ed25519.Sign(vendorRoot, attest.GPUCertMessage(nodeID, i, pub))),
		}
	}
	return a
}

func (a *attester) akPub() ed25519.PublicKey {
	return publicOf(a.ak)
}

func (a *attester) evidence(nonce string, tampered TamperSet) (attest.Evidence, error) {
	log := eventLog(tampered)
	pcrs, err := attest.ReplayPCRs(log)
	if err != nil {
		return attest.Evidence{}, fmt.Errorf("replay event log: %w", err)
	}
	signer := a.ak
	if tampered[TamperAK] {
		signer = a.forgedAK
	}
	quote := ed25519.Sign(signer, attest.QuoteMessage(a.nodeID, nonce, pcrs))
	return attest.Evidence{
		NodeID:   a.nodeID,
		Nonce:    nonce,
		PCRs:     formatPCRs(pcrs),
		EventLog: log,
		QuoteSig: b64(quote),
		GPUs:     a.gpuReports(nonce, tampered),
	}, nil
}

func (a *attester) gpuReports(nonce string, tampered TamperSet) []attest.GPUReport {
	vbios := DefaultVBIOS
	if tampered[TamperGPUVBIOS] {
		vbios = TamperedVBIOS
	}
	ccMode := attest.CCModeOn
	if tampered[TamperCCMode] {
		ccMode = attest.CCModeOff
	}
	reports := make([]attest.GPUReport, len(a.gpus))
	for i, g := range a.gpus {
		report := attest.GPUReportMessage(a.nodeID, nonce, i, vbios, ccMode)
		reports[i] = attest.GPUReport{
			Index:         i,
			VBIOS:         vbios,
			CCMode:        ccMode,
			DevicePub:     g.pubB64,
			DeviceCertSig: g.certSig,
			ReportSig:     b64(ed25519.Sign(g.key, report)),
		}
	}
	return reports
}

func eventLog(tampered TamperSet) []attest.Event {
	log := make([]attest.Event, 0, len(attest.Measured))
	for _, m := range attest.Measured {
		name := DefaultName(m.Component)
		digest := attest.ComponentDigest(name)
		if tampered[Tamper(m.Component)] {
			name, digest = unknownName, attest.TamperedDigest(m.Component)
		}
		log = append(log, attest.Event{
			PCR:       m.PCR,
			Component: m.Component,
			Name:      name,
			SHA256:    hex.EncodeToString(digest),
		})
	}
	return log
}

func formatPCRs(pcrs map[int][]byte) map[string]string {
	out := make(map[string]string, len(pcrs))
	for idx, val := range pcrs {
		out[strconv.Itoa(idx)] = hex.EncodeToString(val)
	}
	return out
}

func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
