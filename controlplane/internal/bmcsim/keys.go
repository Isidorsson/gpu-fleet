package bmcsim

import (
	"crypto/ed25519"
	"crypto/sha256"
	"strconv"
)

// Key derivation labels from docs/attestation.md "Simulator key derivation".
const (
	akLabel         = "gpu-fleet/bmcsim/ak/"
	forgedAKLabel   = "gpu-fleet/bmcsim/ak-tampered/"
	vendorRootLabel = "gpu-fleet/bmcsim/vendor-root/"
	gpuLabel        = "gpu-fleet/bmcsim/gpu/"
)

// VendorRootPublic is the GPU vendor root every simulated GPU certificate chains to.
func VendorRootPublic(seed uint64) ed25519.PublicKey {
	return publicOf(vendorRootKey(seed))
}

// AKPublic is the genuine attestation key of a node, the one recorded at racking.
func AKPublic(seed uint64, nodeID string) ed25519.PublicKey {
	return publicOf(akKey(seed, nodeID))
}

func vendorRootKey(seed uint64) ed25519.PrivateKey {
	return seededKey(vendorRootLabel + decimal(seed))
}

func akKey(seed uint64, nodeID string) ed25519.PrivateKey {
	return seededKey(akLabel + decimal(seed) + "/" + nodeID)
}

func forgedAKKey(seed uint64, nodeID string) ed25519.PrivateKey {
	return seededKey(forgedAKLabel + decimal(seed) + "/" + nodeID)
}

func gpuKey(seed uint64, nodeID string, index int) ed25519.PrivateKey {
	return seededKey(gpuLabel + decimal(seed) + "/" + nodeID + "/" + strconv.Itoa(index))
}

func seededKey(label string) ed25519.PrivateKey {
	sum := sha256.Sum256([]byte(label))
	return ed25519.NewKeyFromSeed(sum[:])
}

func publicOf(key ed25519.PrivateKey) ed25519.PublicKey {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		panic("ed25519 private key returned a non-ed25519 public key")
	}
	return pub
}

func decimal(seed uint64) string {
	return strconv.FormatUint(seed, 10)
}
