package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/remediation"
)

var testLimit = remediation.Limit{Max: 5, Window: 10 * time.Minute}

func decisionFor(nodeID string) remediation.Decision {
	return remediation.Decision{
		NodeID: nodeID, AlertName: "GPUFatalXid", Fingerprint: "fp-" + nodeID, Action: remediation.ActionQuarantine,
	}
}

func remediate(t *testing.T, p *Postgres, nodeID string, limit remediation.Limit, now time.Time) remediation.Outcome {
	t.Helper()
	outcome, err := p.ApplyRemediation(context.Background(), decisionFor(nodeID), limit, now)
	if err != nil {
		t.Fatalf("ApplyRemediation(%s): %v", nodeID, err)
	}
	return outcome
}

func mustState(t *testing.T, p *Postgres, nodeID string, want node.State) {
	t.Helper()
	got, err := p.Get(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get %s: %v", nodeID, err)
	}
	if got.State != want {
		t.Fatalf("node %s state = %s, want %s", nodeID, got.State, want)
	}
}

func TestRemediationLockClassIsDistinct(t *testing.T) {
	if remediationLockClass == reservationWriteLockClass {
		t.Fatalf("remediation and reservation writes share advisory lock class %#x", remediationLockClass)
	}
}

func TestRemediationQuarantinesEveryStateThatTakesAHardwareFault(t *testing.T) {
	for _, state := range []node.State{
		node.StateInspecting, node.StateAvailable, node.StateProvisioning, node.StateActive, node.StateWiping,
	} {
		t.Run(string(state), func(t *testing.T) {
			p := openTestStore(t)
			registerIn(t, p, "n1", state)
			before := eventCount(t, p, "n1")

			if got := remediate(t, p, "n1", testLimit, testNow); got != remediation.OutcomeQuarantined {
				t.Fatalf("outcome = %s, want quarantined", got)
			}
			mustState(t, p, "n1", node.StateQuarantined)

			events, err := p.Events(context.Background(), "n1")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if len(events) != before+1 {
				t.Fatalf("got %d events, want %d", len(events), before+1)
			}
			last := events[len(events)-1]
			if last.Event != node.EventHardwareFault || last.From != state || last.To != node.StateQuarantined {
				t.Fatalf("last event = %+v, want hardware_fault %s -> quarantined", last, state)
			}
		})
	}
}

func TestRemediationKeepsTheTenantOfAnActiveNode(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateActive)

	if got := remediate(t, p, "n1", testLimit, testNow); got != remediation.OutcomeQuarantined {
		t.Fatalf("outcome = %s, want quarantined", got)
	}

	n, err := p.Get(ctx, "n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n.State != node.StateQuarantined || n.Tenant != "acme" {
		t.Fatalf("node = %+v, want quarantined and still held by acme until a wipe", n)
	}
	events, err := p.Events(ctx, "n1")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	last := events[len(events)-1]
	if last.Event != node.EventHardwareFault || last.Tenant != "acme" {
		t.Fatalf("last event = %+v, want hardware_fault recorded for acme", last)
	}

	if _, err := p.ApplyEvent(ctx, "n1", node.EventRepaired, ""); err != nil {
		t.Fatalf("repaired: %v", err)
	}
	if _, err := p.ApplyEvent(ctx, "n1", node.EventWipeSucceeded, ""); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if n, err = p.Get(ctx, "n1"); err != nil {
		t.Fatalf("get after wipe: %v", err)
	}
	if n.Tenant != "" {
		t.Fatalf("tenant after wipe = %q, want none", n.Tenant)
	}
}

func TestRemediationIgnoresTheNodesLease(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateWiping)
	if _, err := p.ClaimWork(context.Background(), "worker-1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if got := remediate(t, p, "n1", testLimit, testNow); got != remediation.OutcomeQuarantined {
		t.Fatalf("outcome = %s, want quarantined despite the live lease", got)
	}
	mustState(t, p, "n1", node.StateQuarantined)
}

func TestRemediationOutcomesWithoutAQuarantine(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "racked", node.StateRacked)
	registerIn(t, p, "held", node.StateQuarantined)
	registerIn(t, p, "ok", node.StateAvailable)
	heldEvents := eventCount(t, p, "held")

	tests := []struct {
		nodeID string
		want   remediation.Outcome
	}{
		{"missing", remediation.OutcomeUnknownNode},
		{"held", remediation.OutcomeAlreadyQuarantined},
		{"racked", remediation.OutcomeNotApplicable},
	}
	for _, tt := range tests {
		if got := remediate(t, p, tt.nodeID, testLimit, testNow); got != tt.want {
			t.Errorf("remediate(%s) = %s, want %s", tt.nodeID, got, tt.want)
		}
	}

	mustState(t, p, "racked", node.StateRacked)
	mustState(t, p, "held", node.StateQuarantined)
	mustState(t, p, "ok", node.StateAvailable)
	if got := eventCount(t, p, "held"); got != heldEvents {
		t.Errorf("already quarantined node gained %d events, want none", got-heldEvents)
	}
	if got := eventCount(t, p, "racked"); got != 0 {
		t.Errorf("racked node has %d events, want none", got)
	}
}

func TestRemediationRecordsEveryOutcomeWithItsFields(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "racked", node.StateRacked)
	registerIn(t, p, "held", node.StateQuarantined)
	registerIn(t, p, "a", node.StateAvailable)
	registerIn(t, p, "b", node.StateAvailable)
	limit := remediation.Limit{Max: 1, Window: time.Hour}

	steps := []struct {
		nodeID string
		want   remediation.Outcome
	}{
		{"a", remediation.OutcomeQuarantined},
		{"b", remediation.OutcomeRateLimited},
		{"held", remediation.OutcomeAlreadyQuarantined},
		{"racked", remediation.OutcomeNotApplicable},
		{"missing", remediation.OutcomeUnknownNode},
	}
	for i, s := range steps {
		if got := remediate(t, p, s.nodeID, limit, at(time.Duration(i)*time.Second)); got != s.want {
			t.Fatalf("remediate(%s) = %s, want %s", s.nodeID, got, s.want)
		}
	}

	records, err := p.ListRemediations(context.Background(), 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != len(steps) {
		t.Fatalf("got %d records, want %d: %+v", len(records), len(steps), records)
	}
	for i, s := range steps {
		got := records[len(steps)-1-i]
		want := remediation.Record{
			NodeID: s.nodeID, AlertName: "GPUFatalXid", Fingerprint: "fp-" + s.nodeID,
			Action: remediation.ActionQuarantine, Outcome: s.want, At: at(time.Duration(i) * time.Second),
		}
		if !got.At.Equal(want.At) {
			t.Errorf("record %d at = %v, want %v", i, got.At, want.At)
		}
		got.At, want.At = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("record %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestRemediationRateLimitWindowBoundary(t *testing.T) {
	p := openTestStore(t)
	limit := remediation.Limit{Max: 2, Window: 10 * time.Minute}
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		registerIn(t, p, id, node.StateAvailable)
	}

	steps := []struct {
		name   string
		nodeID string
		at     time.Duration
		want   remediation.Outcome
	}{
		{"first quarantine", "n1", 0, remediation.OutcomeQuarantined},
		{"second quarantine", "n2", time.Minute, remediation.OutcomeQuarantined},
		{"third within the window", "n3", 2 * time.Minute, remediation.OutcomeRateLimited},
		{"one microsecond before the first expires", "n3", 10*time.Minute - time.Microsecond, remediation.OutcomeRateLimited},
		{"the first is exactly a window old", "n3", 10 * time.Minute, remediation.OutcomeQuarantined},
		{"window holds the second and third", "n4", 10*time.Minute + 30*time.Second, remediation.OutcomeRateLimited},
		{"second is exactly a window old", "n4", 11 * time.Minute, remediation.OutcomeQuarantined},
		{"all earlier ones expired", "n5", 40 * time.Minute, remediation.OutcomeQuarantined},
	}
	for _, s := range steps {
		if got := remediate(t, p, s.nodeID, limit, at(s.at)); got != s.want {
			t.Fatalf("%s: outcome = %s, want %s", s.name, got, s.want)
		}
	}
}

func TestRemediationOnlyQuarantinesCountAgainstTheLimit(t *testing.T) {
	p := openTestStore(t)
	limit := remediation.Limit{Max: 2, Window: time.Hour}
	registerIn(t, p, "held", node.StateQuarantined)
	registerIn(t, p, "racked", node.StateRacked)
	registerIn(t, p, "a", node.StateAvailable)
	registerIn(t, p, "b", node.StateAvailable)
	registerIn(t, p, "c", node.StateAvailable)

	remediate(t, p, "held", limit, at(0))
	remediate(t, p, "racked", limit, at(0))
	remediate(t, p, "missing", limit, at(0))
	remediate(t, p, "a", limit, at(time.Second))
	remediate(t, p, "c", limit, at(2*time.Second))
	remediate(t, p, "c", limit, at(3*time.Second))
	if got := remediate(t, p, "b", limit, at(4*time.Second)); got != remediation.OutcomeRateLimited {
		t.Fatalf("b = %s, want rate_limited after two real quarantines", got)
	}
	remediate(t, p, "b", limit, at(5*time.Second))
	mustState(t, p, "b", node.StateAvailable)
}

func TestRemediationRefusalsComeBeforeTheRateLimit(t *testing.T) {
	p := openTestStore(t)
	limit := remediation.Limit{Max: 1, Window: time.Hour}
	registerIn(t, p, "spent", node.StateAvailable)
	registerIn(t, p, "held", node.StateQuarantined)
	registerIn(t, p, "racked", node.StateRacked)
	registerIn(t, p, "ok", node.StateAvailable)

	if got := remediate(t, p, "spent", limit, at(0)); got != remediation.OutcomeQuarantined {
		t.Fatalf("spent = %s, want quarantined", got)
	}
	for _, tt := range []struct {
		nodeID string
		want   remediation.Outcome
	}{
		{"missing", remediation.OutcomeUnknownNode},
		{"held", remediation.OutcomeAlreadyQuarantined},
		{"racked", remediation.OutcomeNotApplicable},
		{"ok", remediation.OutcomeRateLimited},
	} {
		if got := remediate(t, p, tt.nodeID, limit, at(time.Second)); got != tt.want {
			t.Errorf("remediate(%s) with the budget spent = %s, want %s", tt.nodeID, got, tt.want)
		}
	}
	mustState(t, p, "ok", node.StateAvailable)
	if got := eventCount(t, p, "ok"); got != 2 {
		t.Errorf("rate limited node has %d events, want only its 2 setup events", got)
	}
}

func TestConcurrentRemediationsShareOneLimit(t *testing.T) {
	const (
		nodes  = 10
		budget = 5
	)
	p := openTestStoreWithConns(t, nodes+2)
	limit := remediation.Limit{Max: budget, Window: time.Hour}
	ids := registerFleet(t, p, "n", nodes)

	start := make(chan struct{})
	outcomes := make([]remediation.Outcome, nodes)
	errs := make([]error, nodes)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			<-start
			outcomes[i], errs[i] = p.ApplyRemediation(context.Background(), decisionFor(id), limit, testNow)
		})
	}
	close(start)
	wg.Wait()

	counts := map[remediation.Outcome]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("remediation %d: %v", i, err)
		}
		counts[outcomes[i]]++
	}
	if counts[remediation.OutcomeQuarantined] != budget || counts[remediation.OutcomeRateLimited] != nodes-budget {
		t.Fatalf("outcomes = %v, want %d quarantined and %d rate_limited", counts, budget, nodes-budget)
	}

	quarantined, err := p.List(context.Background(), node.StateQuarantined)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(quarantined) != budget {
		t.Fatalf("%d nodes quarantined, want %d", len(quarantined), budget)
	}
	records, err := p.ListRemediations(context.Background(), 100)
	if err != nil || len(records) != nodes {
		t.Fatalf("records = %d, %v; want %d", len(records), err, nodes)
	}
}

func TestConcurrentRemediationsOfOneNodeQuarantineItOnce(t *testing.T) {
	const racers = 10
	p := openTestStoreWithConns(t, racers+2)
	registerIn(t, p, "n1", node.StateActive)
	before := eventCount(t, p, "n1")
	limit := remediation.Limit{Max: racers, Window: time.Hour}

	start := make(chan struct{})
	outcomes := make([]remediation.Outcome, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			<-start
			outcomes[i], errs[i] = p.ApplyRemediation(context.Background(), decisionFor("n1"), limit, testNow)
		})
	}
	close(start)
	wg.Wait()

	counts := map[remediation.Outcome]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("remediation %d: %v", i, err)
		}
		counts[outcomes[i]]++
	}
	if counts[remediation.OutcomeQuarantined] != 1 || counts[remediation.OutcomeAlreadyQuarantined] != racers-1 {
		t.Fatalf("outcomes = %v, want 1 quarantined and %d already_quarantined", counts, racers-1)
	}
	if got := eventCount(t, p, "n1"); got != before+1 {
		t.Fatalf("n1 has %d events, want %d", got, before+1)
	}
}

func TestRemediationRacingATransitionLeavesAConsistentNode(t *testing.T) {
	const rounds = 10
	p := openTestStoreWithConns(t, 6)
	ctx := context.Background()
	limit := remediation.Limit{Max: rounds, Window: time.Hour}

	for i := range rounds {
		id := fmt.Sprintf("n%02d", i)
		registerIn(t, p, id, node.StateActive)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var transitionErr error
		wg.Go(func() {
			<-start
			_, transitionErr = p.ApplyEvent(ctx, id, node.EventRelease, "")
		})
		wg.Go(func() {
			<-start
			if _, err := p.ApplyRemediation(ctx, decisionFor(id), limit, testNow); err != nil {
				t.Errorf("remediate %s: %v", id, err)
			}
		})
		close(start)
		wg.Wait()

		if transitionErr != nil && !errors.Is(transitionErr, node.ErrInvalidTransition) {
			t.Fatalf("release %s: %v", id, transitionErr)
		}
		// Whichever order won, release then fault ends quarantined (wiping takes
		// hardware_fault) and fault then release is refused.
		mustState(t, p, id, node.StateQuarantined)
	}
}

func TestListRemediationsNewestFirstAndBounded(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()

	empty, err := p.ListRemediations(ctx, 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty log: %#v, %v; want empty non-nil slice", empty, err)
	}

	// Inserted out of time order, with two at the same instant: ordering is by
	// when the remediation happened, then by arrival.
	for _, s := range []struct {
		id string
		at time.Duration
	}{{"b", 2 * time.Minute}, {"a", time.Minute}, {"d", 3 * time.Minute}, {"c", 3 * time.Minute}} {
		remediate(t, p, s.id, testLimit, at(s.at))
	}

	records, err := p.ListRemediations(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, r := range records {
		got = append(got, r.NodeID)
	}
	if want := []string{"c", "d", "b", "a"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	two, err := p.ListRemediations(ctx, 2)
	if err != nil || len(two) != 2 || two[0].NodeID != "c" || two[1].NodeID != "d" {
		t.Fatalf("max=2: %+v, %v; want c then d", two, err)
	}
	if _, err := p.ListRemediations(ctx, 0); err == nil {
		t.Fatal("max=0 succeeded, want an error")
	}
	if _, err := p.ListRemediations(ctx, -1); err == nil {
		t.Fatal("max=-1 succeeded, want an error")
	}
}

func TestApplyRemediationRejectsBadRequestsWithoutRecording(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateAvailable)

	unsupported := decisionFor("n1")
	unsupported.Action = "reboot"
	for name, call := range map[string]func() error{
		"zero max": func() error {
			_, err := p.ApplyRemediation(ctx, decisionFor("n1"), remediation.Limit{Window: time.Hour}, testNow)
			return err
		},
		"zero window": func() error {
			_, err := p.ApplyRemediation(ctx, decisionFor("n1"), remediation.Limit{Max: 1}, testNow)
			return err
		},
		"unsupported action": func() error {
			_, err := p.ApplyRemediation(ctx, unsupported, testLimit, testNow)
			return err
		},
		"empty action": func() error {
			_, err := p.ApplyRemediation(ctx, remediation.Decision{NodeID: "n1"}, testLimit, testNow)
			return err
		},
	} {
		if err := call(); !errors.Is(err, remediation.ErrInvalidConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}

	mustState(t, p, "n1", node.StateAvailable)
	records, err := p.ListRemediations(ctx, 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("records = %+v, %v; want none", records, err)
	}
}

func TestApplyRemediationFailsOnAClosedPool(t *testing.T) {
	p := openTestStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	p.pool.Close()

	if outcome, err := p.ApplyRemediation(context.Background(), decisionFor("n1"), testLimit, testNow); err == nil {
		t.Fatalf("outcome = %q, want an error from the closed pool", outcome)
	}
}

func TestRemediationActionsRejectIgnoredOutcome(t *testing.T) {
	p := openTestStore(t)
	for _, outcome := range remediation.Outcomes() {
		_, err := p.pool.Exec(context.Background(),
			`INSERT INTO remediation_actions (node_id, alertname, fingerprint, action, outcome, at)
			 VALUES ('n1', 'GPUFatalXid', 'fp', 'quarantine', $1, now())`, string(outcome))
		if wantRejected := outcome == remediation.OutcomeIgnored; (err != nil) != wantRejected {
			t.Errorf("outcome %q: err = %v, want rejected=%v", outcome, err, wantRejected)
		}
	}
	if _, err := p.pool.Exec(context.Background(),
		`INSERT INTO remediation_actions (node_id, alertname, fingerprint, action, outcome, at)
		 VALUES ('n1', 'GPUFatalXid', 'fp', 'quarantine', 'bogus', now())`); err == nil {
		t.Error("unknown outcome accepted, want check violation")
	}
}
