package attestsvc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/node"
)

const (
	nonceBytes = 32

	// ReasonEnrolledKeyInvalid is the failure reason for a node whose
	// recorded attestation key cannot be used, so no challenge is sent.
	ReasonEnrolledKeyInvalid = "enrolled attestation key invalid"
)

// Attest challenges n with a fresh nonce and returns the signed, recorded
// result. A node that fails verification yields a fail result, not an error.
// Errors are infrastructure failures; those from the deploy agent keep their
// deployagent sentinels, so deployagent.ErrTransient still means "retry".
//
// A nonce whose evidence fetch failed is left to expire: it was never
// answered, and it cannot be consumed without evidence.
func (s *Service) Attest(ctx context.Context, n node.Node) (attest.Result, error) {
	akPub, err := decodeAK(n.AKPub)
	if err != nil {
		s.log.Warn("enrolled attestation key invalid", "node", n.ID, "err", err)
		return s.seal(ctx, attest.Result{
			NodeID:           n.ID,
			Verdict:          attest.VerdictFail,
			Reasons:          []string{ReasonEnrolledKeyInvalid},
			ReferenceVersion: s.reference.Version,
		})
	}
	nonce, err := s.issueNonce(ctx, n.ID)
	if err != nil {
		return attest.Result{}, err
	}
	evidence, err := s.dial(n.BMC).Evidence(ctx, n.ID, nonce)
	if err != nil {
		return attest.Result{}, fmt.Errorf("fetch evidence from %s: %w", n.ID, err)
	}
	if err := s.store.ConsumeNonce(ctx, n.ID, nonce, s.clock.Now()); err != nil {
		return attest.Result{}, fmt.Errorf("consume nonce for %s: %w", n.ID, err)
	}
	return s.seal(ctx, s.verify(evidence, attest.Expected{NodeID: n.ID, Nonce: nonce, AKPub: akPub}, s.reference))
}

// Latest returns the most recent signed result recorded for nodeID.
func (s *Service) Latest(ctx context.Context, nodeID string) (attest.Result, error) {
	return s.store.LatestAttestation(ctx, nodeID)
}

func (s *Service) issueNonce(ctx context.Context, nodeID string) (string, error) {
	raw := make([]byte, nonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(raw)
	if err := s.store.IssueNonce(ctx, nodeID, nonce, s.clock.Now().Add(s.nonceTTL)); err != nil {
		return "", fmt.Errorf("issue nonce for %s: %w", nodeID, err)
	}
	return nonce, nil
}

// seal stamps r with the attestation time, signs it and records it.
func (s *Service) seal(ctx context.Context, r attest.Result) (attest.Result, error) {
	r.AttestedAt = s.clock.Now().UTC().Truncate(time.Second)
	r.Signature = ed25519.Sign(s.signingKey, attest.ResultMessage(r))
	if err := s.store.RecordAttestation(ctx, r); err != nil {
		return attest.Result{}, fmt.Errorf("record attestation for %s: %w", r.NodeID, err)
	}
	s.log.Info("attestation recorded", "node", r.NodeID, "verdict", r.Verdict,
		"reasons", len(r.Reasons), "reference", r.ReferenceVersion)
	return r, nil
}

func decodeAK(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("not standard base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
