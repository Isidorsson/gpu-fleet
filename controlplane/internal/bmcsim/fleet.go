package bmcsim

import (
	"crypto/ed25519"
	"fmt"
	"math/rand/v2"
)

type Fleet struct {
	ids       []string
	machines  map[string]*Machine
	attesters map[string]*attester
	vendorPub ed25519.PublicKey
}

func NodeID(index int) string {
	return fmt.Sprintf("node-%03d", index+1)
}

// NewFleet seeds each machine from (seed, index), so a machine's outcomes are
// reproducible no matter how requests to different machines interleave. All
// attestation keys are derived here, once.
func NewFleet(cfg Config, clock Clock) (*Fleet, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	params := cfg.params()
	vendorRoot := vendorRootKey(cfg.Seed)
	f := &Fleet{
		ids:       make([]string, 0, cfg.Nodes),
		machines:  make(map[string]*Machine, cfg.Nodes),
		attesters: make(map[string]*attester, cfg.Nodes),
		vendorPub: publicOf(vendorRoot),
	}
	for i := range cfg.Nodes {
		id := NodeID(i)
		rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)))
		f.ids = append(f.ids, id)
		f.machines[id] = NewMachine(params, clock, rng)
		f.attesters[id] = newAttester(cfg.Seed, id, vendorRoot)
	}
	return f, nil
}

func (f *Fleet) IDs() []string {
	return append([]string(nil), f.ids...)
}

func (f *Fleet) Machine(id string) (*Machine, bool) {
	m, ok := f.machines[id]
	return m, ok
}
