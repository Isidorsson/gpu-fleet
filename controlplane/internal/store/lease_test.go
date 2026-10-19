package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

const (
	longTTL     = time.Minute
	shortTTL    = 100 * time.Millisecond
	waitTimeout = 5 * time.Second
	pollEvery   = 10 * time.Millisecond
)

var workStates = []node.State{
	node.StateRacked, node.StateInspecting, node.StateProvisioning, node.StateWiping,
}

func mustClaim(t *testing.T, p *Postgres, owner string, ttl time.Duration) node.Node {
	t.Helper()
	n, err := p.ClaimWork(context.Background(), owner, ttl)
	if err != nil {
		t.Fatalf("claim as %s: %v", owner, err)
	}
	return n
}

func wantNoWork(t *testing.T, p *Postgres, owner string) {
	t.Helper()
	if n, err := p.ClaimWork(context.Background(), owner, longTTL); !errors.Is(err, ErrNoWork) {
		t.Fatalf("claim as %s = %+v, %v; want ErrNoWork", owner, n, err)
	}
}

func leaseOf(t *testing.T, p *Postgres, id string) (owner string, expires *time.Time) {
	t.Helper()
	err := p.pool.QueryRow(context.Background(),
		`SELECT coalesce(lease_owner, ''), lease_expires_at FROM nodes WHERE id = $1`, id,
	).Scan(&owner, &expires)
	if err != nil {
		t.Fatalf("read lease of %s: %v", id, err)
	}
	return owner, expires
}

// waitExpired blocks until the database clock says id's lease has run out,
// without anyone reclaiming it.
func waitExpired(t *testing.T, p *Postgres, id string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		var expired bool
		err := p.pool.QueryRow(context.Background(),
			`SELECT lease_expires_at <= clock_timestamp() FROM nodes WHERE id = $1`, id).Scan(&expired)
		if err != nil {
			t.Fatalf("check expiry of %s: %v", id, err)
		}
		if expired {
			return
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("lease on %s did not expire within %v", id, waitTimeout)
}

func eventCount(t *testing.T, p *Postgres, id string) int {
	t.Helper()
	events, err := p.Events(context.Background(), id)
	if err != nil {
		t.Fatalf("events of %s: %v", id, err)
	}
	return len(events)
}

func TestClaimWorkRejectsBadArguments(t *testing.T) {
	var p Postgres
	for _, tt := range []struct {
		owner string
		ttl   time.Duration
	}{{"", longTTL}, {"w1", 0}, {"w1", -time.Second}} {
		if _, err := p.ClaimWork(context.Background(), tt.owner, tt.ttl); !errors.Is(err, errBadLease) {
			t.Errorf("ClaimWork(%q, %v) err = %v, want errBadLease", tt.owner, tt.ttl, err)
		}
	}
}

func TestClaimWorkReturnsTheLeasedNode(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateInspecting)

	got := mustClaim(t, p, "w1", longTTL)
	want := node.Node{ID: "n1", Pod: "p1", Rack: "r1", BMC: testBMC("n1"), State: node.StateInspecting}
	if got != want {
		t.Fatalf("claimed %+v, want %+v", got, want)
	}
	if owner, expires := leaseOf(t, p, "n1"); owner != "w1" || expires == nil {
		t.Fatalf("lease = %q, %v; want held by w1", owner, expires)
	}
}

func TestClaimWorkOldestFirst(t *testing.T) {
	p := openTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		registerIn(t, p, id, node.StateRacked)
	}
	for _, want := range []string{"a", "b", "c"} {
		if got := mustClaim(t, p, "w1", longTTL); got.ID != want {
			t.Fatalf("claimed %s, want %s", got.ID, want)
		}
	}
}

func TestClaimWorkSkipsLeasedNodes(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)
	wantNoWork(t, p, "w2")
}

func TestClaimWorkIgnoresNonWorkStates(t *testing.T) {
	p := openTestStore(t)
	for _, state := range []node.State{node.StateAvailable, node.StateActive, node.StateQuarantined} {
		registerIn(t, p, string(state), state)
	}
	wantNoWork(t, p, "w1")

	registerIn(t, p, "zz-racked", node.StateRacked)
	if got := mustClaim(t, p, "w1", longTTL); got.ID != "zz-racked" {
		t.Fatalf("claimed %s, want zz-racked", got.ID)
	}
}

func TestConcurrentClaimsAreUnique(t *testing.T) {
	p := openTestStore(t)
	const nodes, claimers = 20, 8
	for i := range nodes {
		registerIn(t, p, fmt.Sprintf("n%02d", i), workStates[i%len(workStates)])
	}

	claimed := make([][]string, claimers)
	var wg sync.WaitGroup
	for i := range claimers {
		wg.Go(func() {
			owner := fmt.Sprintf("w%d", i)
			for {
				n, err := p.ClaimWork(context.Background(), owner, longTTL)
				if errors.Is(err, ErrNoWork) {
					return
				}
				if err != nil {
					t.Errorf("claim as %s: %v", owner, err)
					return
				}
				claimed[i] = append(claimed[i], n.ID)
			}
		})
	}
	wg.Wait()

	seen := map[string]string{}
	for i, ids := range claimed {
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				t.Errorf("%s claimed by both w%d and %s", id, i, prev)
			}
			seen[id] = fmt.Sprintf("w%d", i)
		}
	}
	if len(seen) != nodes {
		t.Fatalf("%d distinct nodes claimed, want %d", len(seen), nodes)
	}
	wantNoWork(t, p, "late")
}

func TestExpiredLeaseIsReclaimable(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateWiping)
	mustClaim(t, p, "w1", shortTTL)
	waitExpired(t, p, "n1")

	if got := mustClaim(t, p, "w2", longTTL); got.ID != "n1" {
		t.Fatalf("claimed %s, want n1", got.ID)
	}
	if owner, _ := leaseOf(t, p, "n1"); owner != "w2" {
		t.Fatalf("lease owner = %q, want w2", owner)
	}
}

func claimAttemptsOf(t *testing.T, p *Postgres, id string) int {
	t.Helper()
	var n int
	err := p.pool.QueryRow(context.Background(),
		`SELECT claim_attempts FROM nodes WHERE id = $1`, id).Scan(&n)
	if err != nil {
		t.Fatalf("read claim attempts of %s: %v", id, err)
	}
	return n
}

// A worker that keeps dying on the oldest node must not starve the rest.
func TestClaimWorkDefersNodeWhoseWorkerDied(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "poison", node.StateRacked)
	registerIn(t, p, "healthy", node.StateRacked)
	mustClaim(t, p, "w1", shortTTL)
	waitExpired(t, p, "poison")

	if got := mustClaim(t, p, "w2", longTTL); got.ID != "healthy" {
		t.Fatalf("claimed %s, want healthy ahead of the expired poison node", got.ID)
	}
	if got := mustClaim(t, p, "w3", longTTL); got.ID != "poison" {
		t.Fatalf("claimed %s, want poison once nothing else waits", got.ID)
	}
}

func TestClaimWorkDefersNodeReleasedWithoutProgress(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "a", node.StateRacked)
	registerIn(t, p, "b", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)
	if err := p.ReleaseLease(context.Background(), "a", "w1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	if got := mustClaim(t, p, "w2", longTTL); got.ID != "b" {
		t.Fatalf("claimed %s, want b ahead of a", got.ID)
	}
}

func TestClaimWorkCountsAttempts(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	for want := 1; want <= 3; want++ {
		mustClaim(t, p, "w1", shortTTL)
		waitExpired(t, p, "n1")
		if got := claimAttemptsOf(t, p, "n1"); got != want {
			t.Fatalf("claim attempts = %d, want %d", got, want)
		}
	}
}

func TestTransitionResetsClaimAttempts(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		apply func(p *Postgres) (node.Node, error)
	}{
		{"worker", func(p *Postgres) (node.Node, error) {
			return p.ApplyEventAsOwner(ctx, "n1", "w2", node.EventStartInspection, "")
		}},
		{"operator", func(p *Postgres) (node.Node, error) {
			return p.ApplyEvent(ctx, "n1", node.EventStartInspection, "")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := openTestStore(t)
			registerIn(t, p, "n1", node.StateRacked)
			mustClaim(t, p, "w1", shortTTL)
			waitExpired(t, p, "n1")
			mustClaim(t, p, "w2", longTTL)

			if _, err := tt.apply(p); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got := claimAttemptsOf(t, p, "n1"); got != 0 {
				t.Fatalf("claim attempts = %d after a transition, want 0", got)
			}
		})
	}
}

func TestRejectedTransitionKeepsClaimAttempts(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	_, err := p.ApplyEventAsOwner(context.Background(), "n1", "w1", node.EventRelease, "")
	if !errors.Is(err, node.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if got := claimAttemptsOf(t, p, "n1"); got != 1 {
		t.Fatalf("claim attempts = %d, want 1", got)
	}
}

func TestRenewLeaseByHolderExtends(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)
	_, before := leaseOf(t, p, "n1")

	if err := p.RenewLease(context.Background(), "n1", "w1", 10*longTTL); err != nil {
		t.Fatalf("renew: %v", err)
	}
	_, after := leaseOf(t, p, "n1")
	if gained := after.Sub(*before); gained < 5*longTTL {
		t.Fatalf("lease extended by %v, want at least %v", gained, 5*longTTL)
	}
}

func TestRenewLeaseByOtherOwnerIsLost(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)
	_, before := leaseOf(t, p, "n1")

	if err := p.RenewLease(context.Background(), "n1", "w2", 10*longTTL); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew by w2: err = %v, want ErrLeaseLost", err)
	}
	if owner, after := leaseOf(t, p, "n1"); owner != "w1" || !after.Equal(*before) {
		t.Fatalf("lease = %q until %v, want unchanged w1 until %v", owner, after, before)
	}
}

func TestRenewLeaseAfterExpiryIsLost(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", shortTTL)
	waitExpired(t, p, "n1")

	if err := p.RenewLease(context.Background(), "n1", "w1", longTTL); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew after expiry: err = %v, want ErrLeaseLost", err)
	}
}

func TestRenewLeaseWithoutLeaseOrNodeIsLost(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	for _, id := range []string{"n1", "missing"} {
		if err := p.RenewLease(context.Background(), id, "w1", longTTL); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("renew %s: err = %v, want ErrLeaseLost", id, err)
		}
	}
}

func TestApplyEventAsOwnerAppliesAndKeepsLease(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	got, err := p.ApplyEventAsOwner(ctx, "n1", "w1", node.EventStartInspection, "")
	if err != nil || got.State != node.StateInspecting {
		t.Fatalf("first event = %+v, %v; want inspecting", got, err)
	}
	if owner, expires := leaseOf(t, p, "n1"); owner != "w1" || expires == nil {
		t.Fatalf("lease after event = %q, %v; want still held by w1", owner, expires)
	}
	wantNoWork(t, p, "w2")

	got, err = p.ApplyEventAsOwner(ctx, "n1", "w1", node.EventInspectionPassed, "")
	if err != nil || got.State != node.StateAvailable {
		t.Fatalf("second event = %+v, %v; want available", got, err)
	}
	if n := eventCount(t, p, "n1"); n != 2 {
		t.Fatalf("audit log has %d events, want 2", n)
	}
}

func TestApplyEventAsOwnerRejectsWrongOwner(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	_, err := p.ApplyEventAsOwner(context.Background(), "n1", "w2", node.EventStartInspection, "")
	assertUntouched(t, p, "n1", node.StateRacked, err)
}

func TestApplyEventAsOwnerRejectsExpiredLease(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", shortTTL)
	waitExpired(t, p, "n1")

	_, err := p.ApplyEventAsOwner(context.Background(), "n1", "w1", node.EventStartInspection, "")
	assertUntouched(t, p, "n1", node.StateRacked, err)
}

func TestApplyEventAsOwnerRejectsUnleasedNode(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)

	_, err := p.ApplyEventAsOwner(context.Background(), "n1", "w1", node.EventStartInspection, "")
	assertUntouched(t, p, "n1", node.StateRacked, err)
}

func TestApplyEventAsOwnerChecksLeaseBeforeTransition(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	_, err := p.ApplyEventAsOwner(context.Background(), "n1", "w2", node.EventRelease, "")
	assertUntouched(t, p, "n1", node.StateRacked, err)

	_, err = p.ApplyEventAsOwner(context.Background(), "n1", "w1", node.EventRelease, "")
	if !errors.Is(err, node.ErrInvalidTransition) {
		t.Fatalf("invalid event by holder: err = %v, want ErrInvalidTransition", err)
	}
}

func TestApplyEventAsOwnerMissingNode(t *testing.T) {
	p := openTestStore(t)
	_, err := p.ApplyEventAsOwner(context.Background(), "missing", "w1", node.EventStartInspection, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func assertUntouched(t *testing.T, p *Postgres, id string, state node.State, err error) {
	t.Helper()
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	got, getErr := p.Get(context.Background(), id)
	if getErr != nil || got.State != state {
		t.Fatalf("node = %+v, %v; want state %s unchanged", got, getErr, state)
	}
	if n := eventCount(t, p, id); n != 0 {
		t.Fatalf("audit log has %d events, want none", n)
	}
}

func TestPlainApplyEventIgnoresLeases(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	got, err := p.ApplyEvent(context.Background(), "n1", node.EventStartInspection, "")
	if err != nil || got.State != node.StateInspecting {
		t.Fatalf("operator event = %+v, %v; want inspecting despite lease", got, err)
	}
}

func TestReleaseLeaseIsIdempotentAndOwnerOnly(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", longTTL)

	if err := p.ReleaseLease(ctx, "n1", "w2"); err != nil {
		t.Fatalf("release by non-holder: %v", err)
	}
	wantNoWork(t, p, "w3")

	for range 2 {
		if err := p.ReleaseLease(ctx, "n1", "w1"); err != nil {
			t.Fatalf("release by holder: %v", err)
		}
	}
	if err := p.ReleaseLease(ctx, "missing", "w1"); err != nil {
		t.Fatalf("release of missing node: %v", err)
	}
	if got := mustClaim(t, p, "w2", longTTL); got.ID != "n1" {
		t.Fatalf("claimed %s after release, want n1", got.ID)
	}
}

func TestStaleOwnerCannotReleaseSuccessorsLease(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateRacked)
	mustClaim(t, p, "w1", shortTTL)
	waitExpired(t, p, "n1")
	mustClaim(t, p, "w2", longTTL)

	if err := p.ReleaseLease(context.Background(), "n1", "w1"); err != nil {
		t.Fatalf("release by stale owner: %v", err)
	}
	if owner, _ := leaseOf(t, p, "n1"); owner != "w2" {
		t.Fatalf("lease owner = %q, want w2 to keep it", owner)
	}
}
