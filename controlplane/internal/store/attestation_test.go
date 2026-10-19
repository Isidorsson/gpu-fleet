package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/node"
)

var attestNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

func TestAKPubRoundTripsThroughEveryNodeQuery(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "a", node.StateInspecting)
	registerIn(t, p, "b", node.StateRacked)

	got, err := p.Get(ctx, "a")
	if err != nil || got.AKPub != testAK("a") {
		t.Errorf("Get = %+v, %v; want ak %q", got, err, testAK("a"))
	}
	nodes, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, n := range nodes {
		if n.AKPub != testAK(n.ID) {
			t.Errorf("List node %s ak = %q, want %q", n.ID, n.AKPub, testAK(n.ID))
		}
	}
	claimed, err := p.ClaimWork(ctx, "w1", time.Minute)
	if err != nil || claimed.AKPub != testAK(claimed.ID) {
		t.Errorf("ClaimWork = %+v, %v; want ak %q", claimed, err, testAK(claimed.ID))
	}
	moved, err := p.ApplyEvent(ctx, "b", node.EventStartInspection, "")
	if err != nil || moved.AKPub != testAK("b") {
		t.Errorf("ApplyEvent = %+v, %v; want ak %q", moved, err, testAK("b"))
	}
}

func TestConsumeNonceIsSingleUse(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	register(t, p, "n1")
	mustIssue(t, p, "n1", "nonce-1", attestNow.Add(time.Minute))

	if err := p.ConsumeNonce(ctx, "n1", "nonce-1", attestNow); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := p.ConsumeNonce(ctx, "n1", "nonce-1", attestNow); !errors.Is(err, ErrNonceInvalid) {
		t.Fatalf("second consume err = %v, want ErrNonceInvalid", err)
	}
}

func TestConsumeNonceRejectsUnknown(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")

	err := p.ConsumeNonce(context.Background(), "n1", "never-issued", attestNow)
	if !errors.Is(err, ErrNonceInvalid) {
		t.Fatalf("err = %v, want ErrNonceInvalid", err)
	}
}

func TestConsumeNonceRejectsAnotherNodesNonce(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	register(t, p, "n1")
	register(t, p, "n2")
	mustIssue(t, p, "n1", "nonce-1", attestNow.Add(time.Minute))

	if err := p.ConsumeNonce(ctx, "n2", "nonce-1", attestNow); !errors.Is(err, ErrNonceInvalid) {
		t.Fatalf("other node err = %v, want ErrNonceInvalid", err)
	}
	if err := p.ConsumeNonce(ctx, "n1", "nonce-1", attestNow); err != nil {
		t.Fatalf("a rejected foreign attempt must not burn the nonce: %v", err)
	}
}

func TestConsumeNonceExpiry(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	register(t, p, "n1")
	expires := attestNow.Add(time.Minute)
	mustIssue(t, p, "n1", "live", expires)
	mustIssue(t, p, "n1", "at-expiry", expires)
	mustIssue(t, p, "n1", "after", expires)

	if err := p.ConsumeNonce(ctx, "n1", "live", expires.Add(-time.Second)); err != nil {
		t.Errorf("just before expiry: %v", err)
	}
	if err := p.ConsumeNonce(ctx, "n1", "at-expiry", expires); !errors.Is(err, ErrNonceInvalid) {
		t.Errorf("at expiry err = %v, want ErrNonceInvalid", err)
	}
	if err := p.ConsumeNonce(ctx, "n1", "after", expires.Add(time.Hour)); !errors.Is(err, ErrNonceInvalid) {
		t.Errorf("after expiry err = %v, want ErrNonceInvalid", err)
	}
}

func TestConcurrentConsumeNonceOnlyOneWins(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")
	mustIssue(t, p, "n1", "nonce-1", attestNow.Add(time.Minute))

	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() { errs[i] = p.ConsumeNonce(context.Background(), "n1", "nonce-1", attestNow) })
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrNonceInvalid):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d consumers succeeded, want exactly 1", wins)
	}
}

func TestIssueNonceUnknownNode(t *testing.T) {
	p := openTestStore(t)

	err := p.IssueNonce(context.Background(), "ghost", "nonce-1", attestNow.Add(time.Minute))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestIssueNonceRejectsDuplicate(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")
	register(t, p, "n2")
	mustIssue(t, p, "n1", "nonce-1", attestNow.Add(time.Minute))

	err := p.IssueNonce(context.Background(), "n2", "nonce-1", attestNow.Add(time.Minute))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a duplicate error", err)
	}
}

func TestIssueNoncePurgesExpired(t *testing.T) {
	p := openReservationStore(t)
	register(t, p, "n1")
	mustIssue(t, p, "n1", "stale", testNow.Add(-time.Second))
	mustIssue(t, p, "n1", "stale-at-boundary", testNow)
	mustIssue(t, p, "n1", "fresh", testNow.Add(time.Minute))

	rows, err := p.pool.Query(context.Background(), `SELECT nonce FROM attestation_nonces ORDER BY nonce`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var left []string
	for rows.Next() {
		var nonce string
		if err := rows.Scan(&nonce); err != nil {
			t.Fatalf("scan: %v", err)
		}
		left = append(left, nonce)
	}
	if want := []string{"fresh"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("nonces left = %v, want %v", left, want)
	}
}

func TestRecordAndLatestAttestation(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	register(t, p, "n1")
	register(t, p, "n2")

	failed := attestResult("n1", attest.VerdictFail, "bootloader digest not in reference values", "gpu 3 vbios not allowed")
	passed := attestResult("n1", attest.VerdictPass)
	passed.AttestedAt = passed.AttestedAt.Add(time.Hour)
	other := attestResult("n2", attest.VerdictFail, "quote signature invalid")
	for _, r := range []attest.Result{failed, other, passed} {
		if err := p.RecordAttestation(ctx, r); err != nil {
			t.Fatalf("record %s: %v", r.NodeID, err)
		}
	}

	if got, err := p.LatestAttestation(ctx, "n1"); err != nil || !reflect.DeepEqual(got, passed) {
		t.Errorf("latest n1 = %+v, %v; want %+v", got, err, passed)
	}
	if got, err := p.LatestAttestation(ctx, "n2"); err != nil || !reflect.DeepEqual(got, other) {
		t.Errorf("latest n2 = %+v, %v; want %+v", got, err, other)
	}
}

func TestLatestAttestationKeepsReasonOrderAndUTC(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	register(t, p, "n1")
	want := attestResult("n1", attest.VerdictFail, "b second", "a first", "c third")
	want.AttestedAt = attestNow.In(time.FixedZone("x", 3*3600))
	if err := p.RecordAttestation(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := p.LatestAttestation(ctx, "n1")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if !reflect.DeepEqual(got.Reasons, want.Reasons) {
		t.Errorf("reasons = %v, want %v", got.Reasons, want.Reasons)
	}
	if got.AttestedAt.Location() != time.UTC || !got.AttestedAt.Equal(want.AttestedAt) {
		t.Errorf("attested_at = %v, want %v in UTC", got.AttestedAt, want.AttestedAt)
	}
}

func TestLatestAttestationNotFound(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")

	for _, id := range []string{"n1", "ghost"} {
		if _, err := p.LatestAttestation(context.Background(), id); !errors.Is(err, ErrAttestationNotFound) {
			t.Errorf("latest %s err = %v, want ErrAttestationNotFound", id, err)
		}
	}
}

func TestRecordAttestationRejectsBadRows(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")
	noSignature := attestResult("n1", attest.VerdictPass)
	noSignature.Signature = nil

	cases := map[string]struct {
		r    attest.Result
		want error
	}{
		"unknown node":    {attestResult("ghost", attest.VerdictPass), ErrNotFound},
		"unknown verdict": {attestResult("n1", "maybe"), nil},
		"no signature":    {noSignature, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := p.RecordAttestation(context.Background(), tc.r)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func mustIssue(t *testing.T, p *Postgres, nodeID, nonce string, expires time.Time) {
	t.Helper()
	if err := p.IssueNonce(context.Background(), nodeID, nonce, expires); err != nil {
		t.Fatalf("issue %s: %v", nonce, err)
	}
}

func attestResult(nodeID string, verdict attest.Verdict, reasons ...string) attest.Result {
	return attest.Result{
		NodeID:             nodeID,
		Verdict:            verdict,
		Reasons:            reasons,
		Nonce:              "nonce-" + nodeID,
		MeasurementsDigest: "digest-" + nodeID,
		ReferenceVersion:   "2026-10-19.1",
		AttestedAt:         attestNow,
		Signature:          []byte("sig-" + nodeID),
	}
}
