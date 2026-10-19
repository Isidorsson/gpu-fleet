package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/attestsvc"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/store"
)

// defaultReferencePath assumes the control plane runs from controlplane/, as
// the README's develop commands do.
const defaultReferencePath = "../deploy/attestation/reference.json"

// newAttestation builds the attestation service. ATTEST_SIGNING_KEY (base64
// Ed25519 seed) is required: results signed with a throwaway key could not be
// verified after a restart. ATTEST_REFERENCE overrides the reference values.
func newAttestation(db *store.Postgres, creds bmcCredentials, log *slog.Logger) (*attestsvc.Service, error) {
	key, err := signingKeyFromEnv()
	if err != nil {
		return nil, err
	}
	path := os.Getenv("ATTEST_REFERENCE")
	if path == "" {
		path = defaultReferencePath
	}
	ref, err := attest.LoadReference(path)
	if err != nil {
		return nil, err
	}
	log.Info("attestation reference values loaded", "path", path, "version", ref.Version)
	return attestsvc.New(
		attestsvc.Config{SigningKey: key, Reference: ref},
		attestsvc.Deps{
			Store: db,
			Dial: func(url string) attestsvc.Agent {
				return deployagent.New(url, creds.user, creds.password, nil)
			},
			Verify: attest.Verify,
			Log:    log,
		},
	)
}

func signingKeyFromEnv() (ed25519.PrivateKey, error) {
	raw := os.Getenv("ATTEST_SIGNING_KEY")
	if raw == "" {
		return nil, fmt.Errorf("ATTEST_SIGNING_KEY is required (base64 of a %d-byte Ed25519 seed)", ed25519.SeedSize)
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ATTEST_SIGNING_KEY must be base64 of a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
