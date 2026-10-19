package attestsvc

import (
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/bmcsim"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
)

const (
	e2eSeed     = 7
	e2eUser     = "bmc-admin"
	e2ePassword = "e2e-pass"
	e2eVersion  = "e2e.1"
)

// e2eFixture runs the service against the real simulator attester and the
// real verifier. Only the store is a fake.
type e2eFixture struct {
	svc    *Service
	store  *fakeStore
	agent  *deployagent.Client
	bmc    *redfish.Client
	url    string
	client *http.Client
}

func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	handler, err := bmcsim.NewHandler(bmcsim.Config{Nodes: 3, Seed: e2eSeed, User: e2eUser, Password: e2ePassword}, bmcsim.SystemClock{})
	if err != nil {
		t.Fatalf("bmcsim: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	f := &e2eFixture{
		store:  &fakeStore{log: &callLog{}},
		agent:  deployagent.New(srv.URL, e2eUser, e2ePassword, srv.Client()),
		bmc:    redfish.New(srv.URL, e2eUser, e2ePassword, srv.Client()),
		url:    srv.URL,
		client: srv.Client(),
	}
	_, key := mustKey(t, 1)
	f.svc, err = New(
		Config{SigningKey: key, Reference: bmcsim.ReferenceValues(e2eSeed, e2eVersion)},
		Deps{
			Store:  f.store,
			Dial:   func(string) Agent { return f.agent },
			Verify: attest.Verify,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// enrolled racks node index i the way deploy/seed.sh does: read its identity
// once and record that key. Machines start powered off.
func (f *e2eFixture) enrolled(t *testing.T, i int) node.Node {
	t.Helper()
	id := bmcsim.NodeID(i)
	ak, err := f.agent.Identity(t.Context(), id)
	if err != nil {
		t.Fatalf("identity %s: %v", id, err)
	}
	return node.Node{ID: id, BMC: f.url, AKPub: ak, State: node.StateInspecting}
}

// running is enrolled plus powered on, which is when the ramdisk that produces
// evidence is up.
func (f *e2eFixture) running(t *testing.T, i int) node.Node {
	t.Helper()
	n := f.enrolled(t, i)
	if err := f.bmc.Reset(t.Context(), n.ID, redfish.ResetOn); err != nil {
		t.Fatalf("power on %s: %v", n.ID, err)
	}
	return n
}

func (f *e2eFixture) tamper(t *testing.T, id, component string) {
	t.Helper()
	body := strings.NewReader(`{"component":"` + component + `"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url+"/sim/systems/"+id+"/tamper", body)
	if err != nil {
		t.Fatalf("tamper request: %v", err)
	}
	req.SetBasicAuth(e2eUser, e2ePassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("tamper %s %s: %v", id, component, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("tamper %s %s: status %d", id, component, resp.StatusCode)
	}
}

func (f *e2eFixture) attest(t *testing.T, n node.Node) attest.Result {
	t.Helper()
	res, err := f.svc.Attest(t.Context(), n)
	if err != nil {
		t.Fatalf("Attest %s: %v", n.ID, err)
	}
	if !ed25519.Verify(f.svc.PublicKey(), attest.ResultMessage(res), res.Signature) {
		t.Errorf("result for %s is not validly signed", n.ID)
	}
	return res
}

func TestE2EUntamperedNodePasses(t *testing.T) {
	f := newE2EFixture(t)

	res := f.attest(t, f.running(t, 0))

	if res.Verdict != attest.VerdictPass || len(res.Reasons) != 0 {
		t.Fatalf("result = %+v, want a pass", res)
	}
	if res.ReferenceVersion != e2eVersion || res.MeasurementsDigest == "" || res.Nonce != f.store.issued[0].nonce {
		t.Errorf("result = %+v, want reference %s, a digest and the issued nonce", res, e2eVersion)
	}
}

func TestE2ETamperedNodeFailsWithReasons(t *testing.T) {
	for _, component := range []string{"firmware", "bmc", "bootloader", "kernel", "gpu_vbios", "cc_mode", "ak"} {
		t.Run(component, func(t *testing.T) {
			f := newE2EFixture(t)
			n := f.running(t, 1)
			f.tamper(t, n.ID, component)

			res := f.attest(t, n)

			if res.Verdict != attest.VerdictFail || len(res.Reasons) == 0 {
				t.Fatalf("result = %+v, want a fail with reasons", res)
			}
			if f.store.recorded[len(f.store.recorded)-1].Verdict != attest.VerdictFail {
				t.Error("fail result was not recorded")
			}
		})
	}
}

func TestE2ESwappedEnrolledKeyFails(t *testing.T) {
	f := newE2EFixture(t)
	n := f.running(t, 0)
	n.AKPub = f.enrolled(t, 1).AKPub

	res := f.attest(t, n)

	if res.Verdict != attest.VerdictFail {
		t.Fatalf("result = %+v, want a fail: the quote is signed by another key", res)
	}
}

func TestE2EClearedTamperPassesAgain(t *testing.T) {
	f := newE2EFixture(t)
	n := f.running(t, 2)
	f.tamper(t, n.ID, "bootloader")
	if res := f.attest(t, n); res.Verdict != attest.VerdictFail {
		t.Fatalf("tampered result = %+v, want a fail", res)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, f.url+"/sim/systems/"+n.ID+"/tamper", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.SetBasicAuth(e2eUser, e2ePassword)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("clear tamper: %v", err)
	}
	resp.Body.Close()

	if res := f.attest(t, n); res.Verdict != attest.VerdictPass {
		t.Fatalf("after reflash result = %+v, want a pass", res)
	}
}

func TestE2EPoweredOffNodeIsAPlainError(t *testing.T) {
	f := newE2EFixture(t)
	n := f.running(t, 0)
	if err := f.bmc.Reset(t.Context(), n.ID, redfish.ResetForceOff); err != nil {
		t.Fatalf("power off: %v", err)
	}

	_, err := f.svc.Attest(t.Context(), n)

	if err == nil || errors.Is(err, deployagent.ErrTransient) {
		t.Fatalf("err = %v, want a non-transient error", err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("err = %v, want the 409 status in it", err)
	}
	if len(f.store.recorded) != 0 {
		t.Errorf("recorded %d results for a node that could not be challenged", len(f.store.recorded))
	}
}
