package chaos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/placement"
)

type execEnv struct {
	rec    *recorder
	exec   *Executor
	agent1 *httptest.Server
	agent2 *httptest.Server
}

type execResponses struct {
	cp, bmc, agent1 respondFunc
}

func newExecEnv(t *testing.T, resp execResponses) *execEnv {
	t.Helper()
	rec := &recorder{}
	bmcResp := resp.bmc
	cp := newFake(t, "cp", rec, resp.cp)
	bmc := newFake(t, "bmc", rec, func(w http.ResponseWriter, c call) {
		if !requireBMCAuth(w, c) {
			return
		}
		if bmcResp != nil {
			bmcResp(w, c)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	agent1 := newFake(t, "agent1", rec, resp.agent1)
	agent2 := newFake(t, "agent2", rec, nil)
	agents := map[string]*Agent{"node-001": NewAgent(agent1.URL), "node-002": NewAgent(agent2.URL)}
	return &execEnv{
		rec:    rec,
		exec:   NewExecutor(NewControlPlane(cp.URL), NewBMC(bmc.URL, testBMCUser, testBMCPassword), agents),
		agent1: agent1,
		agent2: agent2,
	}
}

func status(code int, body string) respondFunc {
	return func(w http.ResponseWriter, _ call) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if body != "" {
			if _, err := w.Write([]byte(body)); err != nil {
				panic(err)
			}
		}
	}
}

func reserveAction() Action {
	return Action{
		Kind: KindReserve, Tenant: "tenant-4", Count: 8, Policy: placement.PolicySamePod,
		StartsAt: t0, EndsAt: t0.Add(4 * time.Minute),
	}
}

func TestExecuteReservePostsTheReservation(t *testing.T) {
	env := newExecEnv(t, execResponses{cp: status(http.StatusCreated, `{"id":"res-abc"}`)})
	outcome, err := env.exec.Execute(context.Background(), reserveAction())
	if err != nil {
		t.Fatal(err)
	}
	c := env.rec.only(t)
	if c.String() != "cp POST /v1/reservations" {
		t.Fatalf("call = %s", c)
	}
	assertJSONEqual(t, c.Body, `{"tenant":"tenant-4","count":8,"policy":"same_pod",`+
		`"starts_at":"2026-10-19T12:00:00Z","ends_at":"2026-10-19T12:04:00Z"}`)
	if c.Auth {
		t.Error("the control plane API takes no credentials")
	}
	if outcome != "created res-abc" {
		t.Errorf("outcome = %q", outcome)
	}
}

func TestExecuteReserveWithoutCapacityIsNotAnError(t *testing.T) {
	body := `{"error":"cannot place 8 nodes with policy same_pod: largest block available is 6","largest":6}`
	env := newExecEnv(t, execResponses{cp: status(http.StatusConflict, body)})
	outcome, err := env.exec.Execute(context.Background(), reserveAction())
	if err != nil {
		t.Fatalf("409 must be tolerated, got %v", err)
	}
	if outcome != "no capacity, largest block 6" {
		t.Errorf("outcome = %q", outcome)
	}
}

func TestExecuteReserveSurfacesOtherFailures(t *testing.T) {
	env := newExecEnv(t, execResponses{cp: status(http.StatusUnprocessableEntity, `{"error":"bad"}`)})
	_, err := env.exec.Execute(context.Background(), reserveAction())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity {
		t.Errorf("want the 422 wrapped, got %v", err)
	}
}

func TestExecuteImplantPostsTamperToBMCSim(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	_, err := env.exec.Execute(context.Background(), Action{Kind: KindImplant, Node: "node-007", Component: "bootloader"})
	if err != nil {
		t.Fatal(err)
	}
	c := env.rec.only(t)
	if c.String() != "bmc POST /sim/systems/node-007/tamper" {
		t.Fatalf("call = %s", c)
	}
	assertJSONEqual(t, c.Body, `{"component":"bootloader"}`)
	if !c.Auth || c.User != testBMCUser || c.Pass != testBMCPassword {
		t.Errorf("basic auth missing: %+v", c)
	}
}

func TestExecuteXIDPostsToThatNodesAgentOnly(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	_, err := env.exec.Execute(context.Background(), Action{Kind: KindXID, Node: "node-001", GPU: 3, Code: 79})
	if err != nil {
		t.Fatal(err)
	}
	c := env.rec.only(t)
	if c.String() != "agent1 POST /fake/xid" {
		t.Fatalf("call = %s", c)
	}
	assertJSONEqual(t, c.Body, `{"gpu":3,"code":79}`)
	if c.Auth {
		t.Error("agents take no credentials")
	}
}

func TestExecuteXIDWithoutAnAgentFailsBeforeAnyCall(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	_, err := env.exec.Execute(context.Background(), Action{Kind: KindXID, Node: "node-099", GPU: 1, Code: 48})
	if err == nil {
		t.Fatal("want an error")
	}
	if calls := env.rec.summary(); len(calls) != 0 {
		t.Errorf("unexpected calls %v", calls)
	}
}

func TestExecuteRepairOfAnAgentBackedNode(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	outcome, err := env.exec.Execute(context.Background(), Action{Kind: KindRepair, Node: "node-001"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"bmc DELETE /sim/systems/node-001/tamper",
		"agent1 DELETE /fake/xid",
		"cp POST /v1/nodes/node-001/events",
	}
	if got := env.rec.summary(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	calls := env.rec.all()
	assertJSONEqual(t, calls[2].Body, `{"event":"repaired"}`)
	if !calls[0].Auth {
		t.Error("clearing tamper needs basic auth")
	}
	if outcome == "" {
		t.Error("want an outcome")
	}
}

func TestExecuteRepairOfAHostWithoutAnAgentSkipsTheAgent(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	if _, err := env.exec.Execute(context.Background(), Action{Kind: KindRepair, Node: "node-010"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"bmc DELETE /sim/systems/node-010/tamper", "cp POST /v1/nodes/node-010/events"}
	if got := env.rec.summary(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestExecuteRepairToleratesANodeThatIsNoLongerQuarantined(t *testing.T) {
	env := newExecEnv(t, execResponses{cp: status(http.StatusConflict, `{"error":"invalid transition"}`)})
	if _, err := env.exec.Execute(context.Background(), Action{Kind: KindRepair, Node: "node-010"}); err != nil {
		t.Errorf("409 on the repaired event must be tolerated, got %v", err)
	}
}

func TestExecuteRepairStopsBeforeTheEventWhenACleanupFails(t *testing.T) {
	tests := []struct {
		name string
		resp execResponses
		want []string
	}{
		{
			"tamper clear fails",
			execResponses{bmc: status(http.StatusInternalServerError, "")},
			[]string{"bmc DELETE /sim/systems/node-001/tamper"},
		},
		{
			"xid clear fails",
			execResponses{agent1: status(http.StatusInternalServerError, "")},
			[]string{"bmc DELETE /sim/systems/node-001/tamper", "agent1 DELETE /fake/xid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newExecEnv(t, tt.resp)
			if _, err := env.exec.Execute(context.Background(), Action{Kind: KindRepair, Node: "node-001"}); err == nil {
				t.Fatal("want an error")
			}
			if got := env.rec.summary(); !slices.Equal(got, tt.want) {
				t.Errorf("calls = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExecuteRepairReportsAFailedEvent(t *testing.T) {
	env := newExecEnv(t, execResponses{cp: status(http.StatusInternalServerError, `{"error":"internal error"}`)})
	if _, err := env.exec.Execute(context.Background(), Action{Kind: KindRepair, Node: "node-010"}); err == nil {
		t.Error("a 500 on the repaired event must fail the repair")
	}
}

func TestExecuteRejectsUnknownKinds(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	if _, err := env.exec.Execute(context.Background(), Action{Kind: "nuke"}); err == nil {
		t.Error("want an error")
	}
}

func TestExecuteHonoursContextCancellation(t *testing.T) {
	env := newExecEnv(t, execResponses{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := env.exec.Execute(ctx, Action{Kind: KindImplant, Node: "node-010", Component: "kernel"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestExecuteReserveSendsUTCTimestamps(t *testing.T) {
	env := newExecEnv(t, execResponses{cp: status(http.StatusCreated, `{"id":"res-abc"}`)})
	zone := time.FixedZone("CEST", 2*60*60)
	a := reserveAction()
	a.StartsAt, a.EndsAt = t0.In(zone), t0.Add(3*time.Minute).In(zone)
	if _, err := env.exec.Execute(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, env.rec.only(t).Body, `{"tenant":"tenant-4","count":8,"policy":"same_pod",`+
		`"starts_at":"2026-10-19T12:00:00Z","ends_at":"2026-10-19T12:03:00Z"}`)
}
