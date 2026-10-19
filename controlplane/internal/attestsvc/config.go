// Package attestsvc runs remote attestation of a node: it challenges the
// node's deploy agent with a fresh nonce, verifies the evidence, and signs and
// records the result. See docs/attestation.md.
package attestsvc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"gpu-fleet/controlplane/internal/attest"
)

// DefaultNonceTTL is how long a challenge stays valid when Config.NonceTTL is
// zero.
const DefaultNonceTTL = 60 * time.Second

var ErrInvalidConfig = errors.New("invalid attestation config")

type Store interface {
	IssueNonce(ctx context.Context, nodeID, nonce string, expires time.Time) error
	ConsumeNonce(ctx context.Context, nodeID, nonce string, now time.Time) error
	RecordAttestation(ctx context.Context, r attest.Result) error
	LatestAttestation(ctx context.Context, nodeID string) (attest.Result, error)
}

// Agent is a node's deploy agent.
type Agent interface {
	Evidence(ctx context.Context, id, nonce string) (attest.Evidence, error)
}

// Verify is attest.Verify; it is injected so the service can be tested alone.
type Verify func(attest.Evidence, attest.Expected, attest.Reference) attest.Result

type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type Config struct {
	// SigningKey signs every result; its public half is published by the API.
	SigningKey ed25519.PrivateKey
	// Reference holds the known-good measurements nodes are verified against.
	Reference attest.Reference
	// NonceTTL is how long a challenge may stay outstanding. Zero means
	// DefaultNonceTTL.
	NonceTTL time.Duration
}

func (c Config) validate() error {
	var errs []error
	if len(c.SigningKey) != ed25519.PrivateKeySize {
		errs = append(errs, fmt.Errorf("%w: SigningKey must be %d bytes, got %d",
			ErrInvalidConfig, ed25519.PrivateKeySize, len(c.SigningKey)))
	} else if !bytes.Equal(ed25519.NewKeyFromSeed(c.SigningKey.Seed()), c.SigningKey) {
		errs = append(errs, fmt.Errorf("%w: SigningKey public half does not match its seed", ErrInvalidConfig))
	}
	if c.Reference.Version == "" {
		errs = append(errs, fmt.Errorf("%w: Reference.Version is required", ErrInvalidConfig))
	}
	if c.NonceTTL < 0 {
		errs = append(errs, fmt.Errorf("%w: NonceTTL must not be negative", ErrInvalidConfig))
	}
	return errors.Join(errs...)
}

type Deps struct {
	Store Store
	// Dial returns the deploy agent client for a node's BMC base URL
	// (node.Node.BMC).
	Dial   func(bmcURL string) Agent
	Verify Verify
	// Clock defaults to the wall clock.
	Clock Clock
	// Log defaults to slog.Default().
	Log *slog.Logger
}

func (d Deps) validate() error {
	if d.Store == nil || d.Dial == nil || d.Verify == nil {
		return fmt.Errorf("%w: Store, Dial and Verify are required", ErrInvalidConfig)
	}
	return nil
}

type Service struct {
	signingKey ed25519.PrivateKey
	reference  attest.Reference
	nonceTTL   time.Duration
	store      Store
	dial       func(bmcURL string) Agent
	verify     Verify
	clock      Clock
	log        *slog.Logger
}

func New(cfg Config, deps Deps) (*Service, error) {
	if err := errors.Join(cfg.validate(), deps.validate()); err != nil {
		return nil, err
	}
	s := &Service{
		signingKey: slices.Clone(cfg.SigningKey),
		reference:  cfg.Reference,
		nonceTTL:   cfg.NonceTTL,
		store:      deps.Store,
		dial:       deps.Dial,
		verify:     deps.Verify,
		clock:      deps.Clock,
		log:        deps.Log,
	}
	if s.nonceTTL == 0 {
		s.nonceTTL = DefaultNonceTTL
	}
	if s.clock == nil {
		s.clock = wallClock{}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// PublicKey returns the key that verifies results this service signs.
func (s *Service) PublicKey() ed25519.PublicKey {
	return ed25519.PublicKey(slices.Clone(s.signingKey[ed25519.SeedSize:]))
}
