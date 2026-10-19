package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gpu-fleet/controlplane/internal/attest"
)

// IssueNonce stores a single-use attestation challenge for nodeID that is
// valid until expires. Nonces already past their expiry are purged in the same
// statement, so challenges whose evidence fetch failed do not pile up.
func (p *Postgres) IssueNonce(ctx context.Context, nodeID, nonce string, expires time.Time) error {
	_, err := p.pool.Exec(ctx,
		`WITH purge AS (DELETE FROM attestation_nonces WHERE expires_at <= $4)
		 INSERT INTO attestation_nonces (nonce, node_id, expires_at) VALUES ($1, $2, $3)`,
		nonce, nodeID, expires, p.now())
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgForeignKeyViolation {
		return fmt.Errorf("%w: %s", ErrNotFound, nodeID)
	}
	if err != nil {
		return fmt.Errorf("issue nonce for %s: %w", nodeID, err)
	}
	return nil
}

// ConsumeNonce deletes nonce and succeeds only if it was issued for nodeID and
// is still live at now. The delete is what makes a nonce single use: of two
// concurrent calls, one finds the row and the other finds nothing. A nonce
// issued for another node is left in place for its rightful holder.
func (p *Postgres) ConsumeNonce(ctx context.Context, nodeID, nonce string, now time.Time) error {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM attestation_nonces WHERE nonce = $1 AND node_id = $2 AND expires_at > $3`,
		nonce, nodeID, now)
	if err != nil {
		return fmt.Errorf("consume nonce for %s: %w", nodeID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: node %s", ErrNonceInvalid, nodeID)
	}
	return nil
}

// RecordAttestation appends a signed result to the node's attestation history.
func (p *Postgres) RecordAttestation(ctx context.Context, r attest.Result) error {
	reasons := r.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO attestations
		     (node_id, verdict, reasons, nonce, measurements_digest, reference_version, attested_at, signature)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.NodeID, string(r.Verdict), reasons, r.Nonce, r.MeasurementsDigest, r.ReferenceVersion,
		r.AttestedAt, r.Signature)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgForeignKeyViolation {
		return fmt.Errorf("%w: %s", ErrNotFound, r.NodeID)
	}
	if err != nil {
		return fmt.Errorf("record attestation for %s: %w", r.NodeID, err)
	}
	return nil
}

// LatestAttestation returns the most recently recorded result for nodeID, or
// ErrAttestationNotFound.
func (p *Postgres) LatestAttestation(ctx context.Context, nodeID string) (attest.Result, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT node_id, verdict, reasons, nonce, measurements_digest, reference_version, attested_at, signature
		 FROM attestations WHERE node_id = $1 ORDER BY id DESC LIMIT 1`, nodeID)
	if err != nil {
		return attest.Result{}, fmt.Errorf("latest attestation for %s: %w", nodeID, err)
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanAttestation)
	if errors.Is(err, pgx.ErrNoRows) {
		return attest.Result{}, fmt.Errorf("%w: %s", ErrAttestationNotFound, nodeID)
	}
	if err != nil {
		return attest.Result{}, fmt.Errorf("scan attestation for %s: %w", nodeID, err)
	}
	return r, nil
}

func scanAttestation(row pgx.CollectableRow) (attest.Result, error) {
	var r attest.Result
	var verdict string
	err := row.Scan(&r.NodeID, &verdict, &r.Reasons, &r.Nonce, &r.MeasurementsDigest,
		&r.ReferenceVersion, &r.AttestedAt, &r.Signature)
	r.Verdict = attest.Verdict(verdict)
	r.AttestedAt = r.AttestedAt.UTC()
	if len(r.Reasons) == 0 {
		r.Reasons = nil
	}
	return r, err
}
