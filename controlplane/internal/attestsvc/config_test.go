package attestsvc

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	_, key := mustKey(t, 1)
	return Config{SigningKey: key, Reference: attest.Reference{Version: testVersion}}
}

func validDeps() Deps {
	return Deps{
		Store:  &fakeStore{log: &callLog{}},
		Dial:   func(string) Agent { return &fakeAgent{log: &callLog{}} },
		Verify: func(attest.Evidence, attest.Expected, attest.Reference) attest.Result { return attest.Result{} },
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	svc, err := New(validConfig(t), validDeps())
	if err != nil {
		t.Fatal(err)
	}
	if svc.nonceTTL != DefaultNonceTTL || DefaultNonceTTL != 60*time.Second {
		t.Errorf("nonceTTL = %v (default %v), want 60s", svc.nonceTTL, DefaultNonceTTL)
	}
	if svc.clock == nil || svc.log == nil {
		t.Errorf("clock %v and log %v must default", svc.clock, svc.log)
	}
}

func TestNewKeepsExplicitNonceTTL(t *testing.T) {
	cfg := validConfig(t)
	cfg.NonceTTL = 5 * time.Second

	svc, err := New(cfg, validDeps())
	if err != nil {
		t.Fatal(err)
	}
	if svc.nonceTTL != 5*time.Second {
		t.Errorf("nonceTTL = %v, want 5s", svc.nonceTTL)
	}
}

func TestNewDoesNotAliasTheCallersKey(t *testing.T) {
	cfg := validConfig(t)
	want := cfg.SigningKey.Public()
	svc, err := New(cfg, validDeps())
	if err != nil {
		t.Fatal(err)
	}

	for i := range cfg.SigningKey {
		cfg.SigningKey[i] = 0
	}

	if !svc.PublicKey().Equal(want) {
		t.Error("zeroing the caller's key changed the service's key")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, otherKey := mustKey(t, 7)
	mismatched := append(ed25519.PrivateKey(nil), validConfig(t).SigningKey...)
	copy(mismatched[ed25519.SeedSize:], otherKey[ed25519.SeedSize:])

	cases := map[string]func(*Config, *Deps){
		"nil key":           func(c *Config, _ *Deps) { c.SigningKey = nil },
		"short key":         func(c *Config, _ *Deps) { c.SigningKey = c.SigningKey[:32] },
		"seed only":         func(c *Config, _ *Deps) { c.SigningKey = c.SigningKey[:ed25519.SeedSize] },
		"mismatched halves": func(c *Config, _ *Deps) { c.SigningKey = mismatched },
		"no ref version":    func(c *Config, _ *Deps) { c.Reference.Version = "" },
		"negative ttl":      func(c *Config, _ *Deps) { c.NonceTTL = -time.Second },
		"no store":          func(_ *Config, d *Deps) { d.Store = nil },
		"no dial":           func(_ *Config, d *Deps) { d.Dial = nil },
		"no verify":         func(_ *Config, d *Deps) { d.Verify = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, deps := validConfig(t), validDeps()
			mutate(&cfg, &deps)

			svc, err := New(cfg, deps)

			if !errors.Is(err, ErrInvalidConfig) || svc != nil {
				t.Fatalf("New = %v, %v; want nil and ErrInvalidConfig", svc, err)
			}
		})
	}
}
