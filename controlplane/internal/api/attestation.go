package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"time"

	"gpu-fleet/controlplane/internal/attest"
)

// signingAlg names the algorithm of the keys GET /v1/attestation/keys lists.
const signingAlg = "Ed25519"

// Attestations serves signed attestation results. The service in
// internal/attestsvc implements it.
type Attestations interface {
	Latest(ctx context.Context, nodeID string) (attest.Result, error)
	PublicKey() ed25519.PublicKey
}

// attestationJSON carries the fields the signature covers, in the encodings
// the signed message uses, so a tenant can rebuild the message offline.
type attestationJSON struct {
	NodeID             string   `json:"node_id"`
	Verdict            string   `json:"verdict"`
	Reasons            []string `json:"reasons"`
	Nonce              string   `json:"nonce"`
	MeasurementsDigest string   `json:"measurements_digest"`
	ReferenceVersion   string   `json:"reference_version"`
	AttestedAt         string   `json:"attested_at"`
	Signature          string   `json:"signature"`
}

type signingKeyJSON struct {
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"`
}

func (s *server) getAttestation(w http.ResponseWriter, r *http.Request) {
	res, err := s.attestations.Latest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAttestationJSON(res))
}

func (s *server) attestationKeys(w http.ResponseWriter, _ *http.Request) {
	key := signingKeyJSON{Alg: signingAlg, PublicKey: base64.StdEncoding.EncodeToString(s.attestations.PublicKey())}
	writeJSON(w, http.StatusOK, map[string][]signingKeyJSON{"keys": {key}})
}

func toAttestationJSON(r attest.Result) attestationJSON {
	reasons := r.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return attestationJSON{
		NodeID:             r.NodeID,
		Verdict:            string(r.Verdict),
		Reasons:            reasons,
		Nonce:              r.Nonce,
		MeasurementsDigest: r.MeasurementsDigest,
		ReferenceVersion:   r.ReferenceVersion,
		AttestedAt:         r.AttestedAt.UTC().Format(time.RFC3339),
		Signature:          base64.StdEncoding.EncodeToString(r.Signature),
	}
}

// validAKPub reports whether raw is the standard base64 of an Ed25519 public
// key.
func validAKPub(raw string) bool {
	key, err := base64.StdEncoding.DecodeString(raw)
	return err == nil && len(key) == ed25519.PublicKeySize
}
