package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

// ControlPlane is the control plane's HTTP API, used as a tenant and an
// operator would.
type ControlPlane struct{ r *rest }

func NewControlPlane(base string) *ControlPlane {
	return &ControlPlane{r: newREST(base, "", "")}
}

// NodeSpec is the body of POST /v1/nodes.
type NodeSpec struct {
	ID    string `json:"id"`
	Pod   string `json:"pod"`
	Rack  string `json:"rack"`
	BMC   string `json:"bmc"`
	AKPub string `json:"ak_pub"`
}

// ReserveRequest is the body of POST /v1/reservations.
type ReserveRequest struct {
	Tenant   string           `json:"tenant"`
	Count    int              `json:"count"`
	Policy   placement.Policy `json:"policy"`
	StartsAt time.Time        `json:"starts_at"`
	EndsAt   time.Time        `json:"ends_at"`
}

// ReserveResult says what POST /v1/reservations answered. Created is false
// when the fleet had no capacity (409), with Largest the biggest block free.
type ReserveResult struct {
	ID      string
	Created bool
	Largest int
}

func (c *ControlPlane) Healthz(ctx context.Context) error {
	return c.r.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// RegisterNode reports created=false when the node already exists (409), so
// registration is idempotent.
func (c *ControlPlane) RegisterNode(ctx context.Context, spec NodeSpec) (created bool, err error) {
	err = c.r.do(ctx, http.MethodPost, "/v1/nodes", spec, nil)
	if hasStatus(err, http.StatusConflict) {
		return false, nil
	}
	return err == nil, err
}

func (c *ControlPlane) Nodes(ctx context.Context) ([]NodeInfo, error) {
	var out struct {
		Nodes []NodeInfo `json:"nodes"`
	}
	if err := c.r.do(ctx, http.MethodGet, "/v1/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

func (c *ControlPlane) Reservations(ctx context.Context) ([]ReservationInfo, error) {
	var out struct {
		Reservations []ReservationInfo `json:"reservations"`
	}
	if err := c.r.do(ctx, http.MethodGet, "/v1/reservations", nil, &out); err != nil {
		return nil, err
	}
	return out.Reservations, nil
}

func (c *ControlPlane) Reserve(ctx context.Context, req ReserveRequest) (ReserveResult, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.r.do(ctx, http.MethodPost, "/v1/reservations", req, &out)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict {
		return ReserveResult{Largest: capacityLargest(se.Body)}, nil
	}
	if err != nil {
		return ReserveResult{}, err
	}
	return ReserveResult{ID: out.ID, Created: true}, nil
}

func capacityLargest(body string) int {
	var out struct {
		Largest int `json:"largest"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return 0
	}
	return out.Largest
}

func (c *ControlPlane) SendEvent(ctx context.Context, nodeID string, ev node.Event) error {
	body := struct {
		Event node.Event `json:"event"`
	}{ev}
	return c.r.do(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(nodeID)+"/events", body, nil)
}

// BMC is bmcsim: the Redfish systems list, the deploy agent's identity and
// the simulator-only tamper switch. All of it needs basic auth.
type BMC struct{ r *rest }

func NewBMC(base, user, pass string) *BMC {
	return &BMC{r: newREST(base, user, pass)}
}

// Systems returns the IDs of every system bmcsim exposes.
func (b *BMC) Systems(ctx context.Context) ([]string, error) {
	var out struct {
		Members []struct {
			ID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := b.r.do(ctx, http.MethodGet, "/redfish/v1/Systems", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Members))
	for _, m := range out.Members {
		ids = append(ids, path.Base(m.ID))
	}
	return ids, nil
}

func (b *BMC) AKPub(ctx context.Context, id string) (string, error) {
	var out struct {
		AKPub string `json:"ak_pub"`
	}
	if err := b.r.do(ctx, http.MethodGet, "/agent/"+url.PathEscape(id)+"/identity", nil, &out); err != nil {
		return "", err
	}
	if out.AKPub == "" {
		return "", fmt.Errorf("identity of %s has no ak_pub", id)
	}
	return out.AKPub, nil
}

func (b *BMC) Tamper(ctx context.Context, id, component string) error {
	body := struct {
		Component string `json:"component"`
	}{component}
	return b.r.do(ctx, http.MethodPost, tamperPath(id), body, nil)
}

func (b *BMC) ClearTamper(ctx context.Context, id string) error {
	return b.r.do(ctx, http.MethodDelete, tamperPath(id), nil, nil)
}

func tamperPath(id string) string {
	return "/sim/systems/" + url.PathEscape(id) + "/tamper"
}

// Agent is one GPU agent's fake-backend control endpoint.
type Agent struct{ r *rest }

func NewAgent(base string) *Agent {
	return &Agent{r: newREST(base, "", "")}
}

const xidPath = "/fake/xid"

func (a *Agent) InjectXID(ctx context.Context, gpu, code int) error {
	body := struct {
		GPU  int `json:"gpu"`
		Code int `json:"code"`
	}{gpu, code}
	return a.r.do(ctx, http.MethodPost, xidPath, body, nil)
}

func (a *Agent) ClearXIDs(ctx context.Context) error {
	return a.r.do(ctx, http.MethodDelete, xidPath, nil, nil)
}
