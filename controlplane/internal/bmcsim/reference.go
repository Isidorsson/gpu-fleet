package bmcsim

import (
	"encoding/hex"

	"gpu-fleet/controlplane/internal/attest"
)

// ReferenceValues are the reference values that accept exactly what an
// untampered simulator with this seed reports. deploy/attestation/reference.json
// is its output for the default seed.
func ReferenceValues(seed uint64, version string) attest.Reference {
	components := make(map[attest.Component][]attest.RefMeasure, len(attest.Measured))
	for _, m := range attest.Measured {
		name := DefaultName(m.Component)
		components[m.Component] = []attest.RefMeasure{{
			Name:   name,
			SHA256: hex.EncodeToString(attest.ComponentDigest(name)),
		}}
	}
	return attest.Reference{
		Version:    version,
		Components: components,
		GPU: attest.RefGPU{
			VendorRootPub: b64(VendorRootPublic(seed)),
			VBIOS:         []string{DefaultVBIOS},
			RequireCCMode: true,
		},
	}
}
