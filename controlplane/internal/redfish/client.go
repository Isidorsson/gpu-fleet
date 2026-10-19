package redfish

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Second

	systemsPath       = "/redfish/v1/Systems"
	resetActionSuffix = "/Actions/ComputerSystem.Reset"

	bootTargetNone      = "None"
	bootTargetPXE       = "Pxe"
	bootEnabledDisabled = "Disabled"
	bootEnabledOnce     = "Once"
)

type Client struct {
	baseURL  string
	user     string
	password string
	hc       *http.Client
}

// New returns a client for the BMC at baseURL. A nil hc gets a client with a
// default timeout.
func New(baseURL, user, password string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		hc:       hc,
	}
}

func (c *Client) Systems(ctx context.Context) ([]string, error) {
	var w systemsWire
	if err := c.do(ctx, http.MethodGet, systemsPath, nil, &w); err != nil {
		return nil, err
	}
	ids, err := w.ids()
	if err != nil {
		return nil, fmt.Errorf("parse GET %s: %w", systemsPath, err)
	}
	return ids, nil
}

func (c *Client) System(ctx context.Context, id string) (System, error) {
	path, err := systemPath(id)
	if err != nil {
		return System{}, err
	}
	var w systemWire
	if err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return System{}, err
	}
	sys, err := w.system(id)
	if err != nil {
		return System{}, fmt.Errorf("parse GET %s: %w", path, err)
	}
	return sys, nil
}

// SetPXEBootOnce makes the next boot of the system PXE boot, once.
func (c *Client) SetPXEBootOnce(ctx context.Context, id string) error {
	path, err := systemPath(id)
	if err != nil {
		return err
	}
	patch := bootPatch{Boot: bootWire{Target: bootTargetPXE, Enabled: bootEnabledOnce}}
	return c.do(ctx, http.MethodPatch, path, patch, nil)
}

func (c *Client) Reset(ctx context.Context, id string, t ResetType) error {
	path, err := systemPath(id)
	if err != nil {
		return err
	}
	if err := validateResetType(t); err != nil {
		return fmt.Errorf("reset system %s: %w", id, err)
	}
	return c.do(ctx, http.MethodPost, path+resetActionSuffix, resetBody{ResetType: t}, nil)
}

func systemPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return systemsPath + "/" + url.PathEscape(id), nil
}

func validateID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("invalid system id %q", id)
	}
	return nil
}

func validateResetType(t ResetType) error {
	switch t {
	case ResetOn, ResetForceOff, ResetForceRestart:
		return nil
	}
	return unknownValue("ResetType", string(t))
}

type systemsWire struct {
	Members []memberWire `json:"Members"`
}

type memberWire struct {
	ODataID string `json:"@odata.id"`
}

type systemWire struct {
	ID         string     `json:"Id"`
	PowerState string     `json:"PowerState"`
	Status     statusWire `json:"Status"`
	Boot       bootWire   `json:"Boot"`
}

type statusWire struct {
	Health string `json:"Health"`
}

type bootWire struct {
	Target  string `json:"BootSourceOverrideTarget"`
	Enabled string `json:"BootSourceOverrideEnabled"`
}

type bootPatch struct {
	Boot bootWire `json:"Boot"`
}

type resetBody struct {
	ResetType ResetType `json:"ResetType"`
}

func (w systemsWire) ids() ([]string, error) {
	ids := make([]string, 0, len(w.Members))
	for _, m := range w.Members {
		id, err := idFromODataID(m.ODataID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func idFromODataID(odataID string) (string, error) {
	segment := odataID[strings.LastIndex(odataID, "/")+1:]
	id, err := url.PathUnescape(segment)
	if err != nil {
		return "", fmt.Errorf("member @odata.id %.*q: %w", maxEchoLen, odataID, err)
	}
	if err := validateID(id); err != nil {
		return "", fmt.Errorf("member @odata.id %.*q: %w", maxEchoLen, odataID, err)
	}
	return id, nil
}

func (w systemWire) system(wantID string) (System, error) {
	if w.ID != wantID {
		return System{}, fmt.Errorf("response Id %.*q does not match requested system", maxEchoLen, w.ID)
	}
	power, err := parsePowerState(w.PowerState)
	if err != nil {
		return System{}, err
	}
	health, err := parseHealth(w.Status.Health)
	if err != nil {
		return System{}, err
	}
	pxeOnce, err := w.Boot.pxeOnce()
	if err != nil {
		return System{}, err
	}
	return System{ID: w.ID, PowerState: power, Health: health, PXEOnce: pxeOnce}, nil
}

func parsePowerState(s string) (PowerState, error) {
	switch p := PowerState(s); p {
	case PowerOn, PowerOff:
		return p, nil
	}
	return "", unknownValue("PowerState", s)
}

func parseHealth(s string) (Health, error) {
	switch h := Health(s); h {
	case HealthOK, HealthCritical:
		return h, nil
	}
	return "", unknownValue("Status.Health", s)
}

func (b bootWire) pxeOnce() (bool, error) {
	if b.Target != bootTargetNone && b.Target != bootTargetPXE {
		return false, unknownValue("BootSourceOverrideTarget", b.Target)
	}
	if b.Enabled != bootEnabledDisabled && b.Enabled != bootEnabledOnce {
		return false, unknownValue("BootSourceOverrideEnabled", b.Enabled)
	}
	return b.Target == bootTargetPXE && b.Enabled == bootEnabledOnce, nil
}

func unknownValue(field, value string) error {
	return fmt.Errorf("unknown %s %.*q", field, maxEchoLen, value)
}
