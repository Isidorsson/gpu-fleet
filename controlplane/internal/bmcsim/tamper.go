package bmcsim

import "maps"

// Tamper names a component an implant has modified. The first four are the
// attest.Measured components under the same names.
type Tamper string

const (
	TamperFirmware   Tamper = "firmware"
	TamperBMC        Tamper = "bmc"
	TamperBootloader Tamper = "bootloader"
	TamperKernel     Tamper = "kernel"
	TamperGPUVBIOS   Tamper = "gpu_vbios"
	TamperCCMode     Tamper = "cc_mode"
	TamperAK         Tamper = "ak"
)

type TamperSet map[Tamper]bool

func ParseTamper(raw string) (Tamper, error) {
	return parseEnum("component", raw,
		TamperFirmware, TamperBMC, TamperBootloader, TamperKernel, TamperGPUVBIOS, TamperCCMode, TamperAK)
}

// AddTamper models an implant: it stays through power cycles and wipes until
// ClearTamper (a reflash).
func (m *Machine) AddTamper(t Tamper) {
	m.begin()
	defer m.mu.Unlock()
	m.tampered[t] = true
}

func (m *Machine) ClearTamper() {
	m.begin()
	defer m.mu.Unlock()
	clear(m.tampered)
}

// AttestationState returns a copy of the tamper set and whether the machine
// is powered on, which is when the ramdisk that produces evidence runs.
func (m *Machine) AttestationState() (tampered TamperSet, powered bool) {
	m.begin()
	defer m.mu.Unlock()
	return maps.Clone(m.tampered), m.power == PowerOn
}
