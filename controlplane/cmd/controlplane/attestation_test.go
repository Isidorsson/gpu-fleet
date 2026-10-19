package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestSigningKeyFromEnv(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	t.Setenv("ATTEST_SIGNING_KEY", base64.StdEncoding.EncodeToString(seed))
	key, err := signingKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !key.Equal(ed25519.NewKeyFromSeed(seed)) {
		t.Fatal("key not derived from the seed")
	}
}

func TestSigningKeyFromEnvRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"unset":     "",
		"not b64":   "%%%",
		"too short": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"full key":  base64.StdEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ATTEST_SIGNING_KEY", raw)
			if _, err := signingKeyFromEnv(); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
