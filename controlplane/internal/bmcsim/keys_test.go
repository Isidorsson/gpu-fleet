package bmcsim

import (
	"crypto/ed25519"
	"crypto/sha256"
	"reflect"
	"strconv"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
)

const (
	keyTestSeed  = 42
	keyTestNodes = 4
)

// specKey restates docs/attestation.md "Simulator key derivation" so the
// production derivation is checked against the spec text, not against itself.
func specKey(label string) ed25519.PrivateKey {
	sum := sha256.Sum256([]byte(label))
	return ed25519.NewKeyFromSeed(sum[:])
}

func newKeyTestFleet(t *testing.T, seed uint64) *Fleet {
	t.Helper()
	f, err := NewFleet(Config{
		Nodes: keyTestNodes, Seed: seed, User: testUser, Password: testPassword,
	}, NewManualClock(testStart))
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	return f
}

func TestKeysFollowTheSpecDerivation(t *testing.T) {
	const node = "node-007"
	if got, want := AKPublic(keyTestSeed, node), publicOf(specKey("gpu-fleet/bmcsim/ak/42/node-007")); !got.Equal(want) {
		t.Fatal("AK does not match sha256(\"gpu-fleet/bmcsim/ak/\" + seed + \"/\" + node_id)")
	}
	if got, want := VendorRootPublic(keyTestSeed), publicOf(specKey("gpu-fleet/bmcsim/vendor-root/42")); !got.Equal(want) {
		t.Fatal("vendor root does not match sha256(\"gpu-fleet/bmcsim/vendor-root/\" + seed)")
	}
	if got, want := gpuKey(keyTestSeed, node, 3), specKey("gpu-fleet/bmcsim/gpu/42/node-007/3"); !got.Equal(want) {
		t.Fatal("GPU key does not match sha256(\"gpu-fleet/bmcsim/gpu/\" + seed + \"/\" + node_id + \"/\" + index)")
	}
	if got, want := forgedAKKey(keyTestSeed, node), specKey("gpu-fleet/bmcsim/ak-tampered/42/node-007"); !got.Equal(want) {
		t.Fatal("forged AK does not match sha256(\"gpu-fleet/bmcsim/ak-tampered/\" + seed + \"/\" + node_id)")
	}
}

func TestKeysAreDeterministicPerSeed(t *testing.T) {
	a, b := newKeyTestFleet(t, keyTestSeed), newKeyTestFleet(t, keyTestSeed)
	if !a.vendorPub.Equal(b.vendorPub) {
		t.Fatal("vendor root differs between fleets with the same seed")
	}
	for _, id := range a.IDs() {
		if !reflect.DeepEqual(a.attesters[id], b.attesters[id]) {
			t.Fatalf("%s: attester differs between fleets with the same seed", id)
		}
	}
}

func TestEveryKeyIsDistinct(t *testing.T) {
	seen := map[string]string{}
	add := func(what string, pub ed25519.PublicKey) {
		t.Helper()
		if prev, dup := seen[string(pub)]; dup {
			t.Fatalf("%s has the same key as %s", what, prev)
		}
		seen[string(pub)] = what
	}
	for _, seed := range []uint64{1, 2} {
		f := newKeyTestFleet(t, seed)
		add("vendor root", f.vendorPub)
		for _, id := range f.IDs() {
			a := f.attesters[id]
			add(id+" AK", a.akPub())
			add(id+" forged AK", publicOf(a.forgedAK))
			for i, g := range a.gpus {
				add(id+" GPU "+strconv.Itoa(i), publicOf(g.key))
			}
		}
	}
	if want := 2 * (1 + keyTestNodes*(2+8)); len(seen) != want {
		t.Fatalf("distinct keys = %d, want %d", len(seen), want)
	}
}

func TestMeasuredComponentsAreTamperNames(t *testing.T) {
	for _, m := range attest.Measured {
		if _, err := ParseTamper(string(m.Component)); err != nil {
			t.Errorf("measured component %q is not a tamper component: %v", m.Component, err)
		}
	}
}
