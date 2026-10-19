package attest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strconv"
	"testing"
)

const (
	testNodeID   = "node-001"
	testNonce    = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	testOldNonce = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	testVBIOS    = "96.00.9F.00.01"
	testRefVer   = "2026-10-19.1"
)

var defaultNames = map[Component]string{
	ComponentFirmware:   "bios-2.4.1",
	ComponentBMC:        "bmc-1.9.3",
	ComponentBootloader: "grub-2.12",
	ComponentKernel:     "inspect-ramdisk-6.8.0",
}

// rig holds the keys of one simulated node and builds evidence signed with
// them. Options change what the attester does before it signs; changes made
// to the returned Evidence afterwards model an attacker editing the wire.
type rig struct {
	tb   testing.TB
	ak   ed25519.PrivateKey
	root ed25519.PrivateKey
	gpus [GPUsPerNode]ed25519.PrivateKey
}

func testKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("gpu-fleet/attest-test/" + label))
	return ed25519.NewKeyFromSeed(seed[:])
}

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey {
	return slices.Clone(ed25519.PublicKey(k[ed25519.SeedSize:]))
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func signB64(k ed25519.PrivateKey, msg []byte) string { return b64(ed25519.Sign(k, msg)) }

func newRig(tb testing.TB) rig {
	tb.Helper()
	r := rig{tb: tb, ak: testKey("ak"), root: testKey("vendor-root")}
	for i := range r.gpus {
		r.gpus[i] = testKey("gpu/" + strconv.Itoa(i))
	}
	return r
}

func (r rig) expected() Expected {
	return Expected{NodeID: testNodeID, Nonce: testNonce, AKPub: pubOf(r.ak)}
}

func (r rig) reference() Reference {
	components := map[Component][]RefMeasure{}
	for _, m := range Measured {
		components[m.Component] = []RefMeasure{refMeasure(defaultNames[m.Component])}
	}
	return Reference{
		Version:    testRefVer,
		Components: components,
		GPU: RefGPU{
			VendorRootPub: b64(pubOf(r.root)),
			VBIOS:         []string{testVBIOS},
			RequireCCMode: true,
		},
	}
}

func refMeasure(name string) RefMeasure {
	return RefMeasure{Name: name, SHA256: hex.EncodeToString(ComponentDigest(name))}
}

type gpuSpec struct {
	GPUReport
	key ed25519.PrivateKey
}

type attesterState struct {
	nodeID string
	nonce  string
	akKey  ed25519.PrivateKey
	log    []Event
	gpus   []gpuSpec
}

type option func(*attesterState)

func (r rig) honest() attesterState {
	s := attesterState{nodeID: testNodeID, nonce: testNonce, akKey: r.ak}
	for _, m := range Measured {
		s.log = append(s.log, componentEvent(m.PCR, m.Component, defaultNames[m.Component]))
	}
	for i := range GPUsPerNode {
		s.gpus = append(s.gpus, gpuSpec{
			GPUReport: GPUReport{Index: i, VBIOS: testVBIOS, CCMode: CCModeOn},
			key:       r.gpus[i],
		})
	}
	return s
}

func componentEvent(pcr int, c Component, name string) Event {
	return Event{PCR: pcr, Component: c, Name: name, SHA256: hex.EncodeToString(ComponentDigest(name))}
}

func (r rig) evidence(opts ...option) Evidence {
	s := r.honest()
	for _, o := range opts {
		o(&s)
	}
	return r.sign(s)
}

func (r rig) sign(s attesterState) Evidence {
	r.tb.Helper()
	ev := Evidence{NodeID: s.nodeID, Nonce: s.nonce, EventLog: s.log}
	r.requote(&ev, s.akKey)
	for _, g := range s.gpus {
		rep := g.GPUReport
		rep.DevicePub = b64(pubOf(g.key))
		rep.DeviceCertSig = signB64(r.root, GPUCertMessage(s.nodeID, rep.Index, rep.DevicePub))
		rep.ReportSig = signB64(g.key, GPUReportMessage(s.nodeID, s.nonce, rep.Index, rep.VBIOS, rep.CCMode))
		ev.GPUs = append(ev.GPUs, rep)
	}
	return ev
}

// requote recomputes the pcrs from the event log and signs a fresh quote, as
// an attacker holding key would after editing the log.
func (r rig) requote(ev *Evidence, key ed25519.PrivateKey) {
	r.tb.Helper()
	pcrs, err := ReplayPCRs(ev.EventLog)
	if err != nil {
		r.tb.Fatalf("replay fixture log: %v", err)
	}
	ev.PCRs = map[string]string{}
	for idx, v := range pcrs {
		ev.PCRs[strconv.Itoa(idx)] = hex.EncodeToString(v)
	}
	ev.QuoteSig = signB64(key, QuoteMessage(ev.NodeID, ev.Nonce, pcrs))
}

func withTampered(c Component) option {
	return func(s *attesterState) {
		i := slices.IndexFunc(s.log, func(e Event) bool { return e.Component == c })
		s.log[i].Name = "unknown"
		s.log[i].SHA256 = hex.EncodeToString(TamperedDigest(c))
	}
}

func withComponent(c Component, name string) option {
	return func(s *attesterState) {
		i := slices.IndexFunc(s.log, func(e Event) bool { return e.Component == c })
		s.log[i] = componentEvent(s.log[i].PCR, c, name)
	}
}

func withNode(id string) option          { return func(s *attesterState) { s.nodeID = id } }
func withNonce(n string) option          { return func(s *attesterState) { s.nonce = n } }
func withAK(k ed25519.PrivateKey) option { return func(s *attesterState) { s.akKey = k } }

func withVBIOS(pos int, v string) option {
	return func(s *attesterState) { s.gpus[pos].VBIOS = v }
}

func withCCMode(pos int, m string) option {
	return func(s *attesterState) { s.gpus[pos].CCMode = m }
}

func withGPUIndex(pos, index int) option {
	return func(s *attesterState) { s.gpus[pos].Index = index }
}

func withoutGPU(pos int) option {
	return func(s *attesterState) { s.gpus = slices.Delete(s.gpus, pos, pos+1) }
}

func withExtraGPU(index int) option {
	return func(s *attesterState) {
		s.gpus = append(s.gpus, gpuSpec{
			GPUReport: GPUReport{Index: index, VBIOS: testVBIOS, CCMode: CCModeOn},
			key:       testKey("extra-gpu"),
		})
	}
}
