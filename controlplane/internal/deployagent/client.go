package deployagent

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

	agentPath    = "/agent"
	taskSuffix   = "/task"
	statusSuffix = "/status"
)

type Client struct {
	baseURL  string
	user     string
	password string
	hc       *http.Client
}

// New returns a client for the deploy agent at baseURL. A nil hc gets a
// client with a default timeout.
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

// SetTask selects what the next PXE boot of the machine runs.
func (c *Client) SetTask(ctx context.Context, id string, t Task) error {
	path, err := agentURLPath(id, taskSuffix)
	if err != nil {
		return err
	}
	if _, err := parseTask(string(t)); err != nil {
		return fmt.Errorf("set task for %s: %w", id, err)
	}
	return c.do(ctx, http.MethodPut, path, taskBody{Task: t}, nil)
}

func (c *Client) Status(ctx context.Context, id string) (Status, error) {
	path, err := agentURLPath(id, statusSuffix)
	if err != nil {
		return Status{}, err
	}
	var w statusWire
	if err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return Status{}, err
	}
	st, err := w.status()
	if err != nil {
		return Status{}, fmt.Errorf("parse GET %s: %w", path, err)
	}
	return st, nil
}

func agentURLPath(id, suffix string) (string, error) {
	if id == "" || id == "." || id == ".." {
		return "", fmt.Errorf("invalid system id %q", id)
	}
	return agentPath + "/" + url.PathEscape(id) + suffix, nil
}

type taskBody struct {
	Task Task `json:"task"`
}

type statusWire struct {
	Task  string `json:"task"`
	Phase string `json:"phase"`
}

func (w statusWire) status() (Status, error) {
	phase, err := parsePhase(w.Phase)
	if err != nil {
		return Status{}, err
	}
	if w.Task == "" {
		return Status{Phase: phase}, nil
	}
	task, err := parseTask(w.Task)
	if err != nil {
		return Status{}, err
	}
	return Status{Task: task, Phase: phase}, nil
}

func parseTask(s string) (Task, error) {
	switch t := Task(s); t {
	case TaskInspect, TaskWipe, TaskDeploy:
		return t, nil
	}
	return "", unknownValue("task", s)
}

func parsePhase(s string) (Phase, error) {
	switch p := Phase(s); p {
	case PhaseIdle, PhaseRunning, PhaseSucceeded, PhaseFailed:
		return p, nil
	}
	return "", unknownValue("phase", s)
}

func unknownValue(field, value string) error {
	return fmt.Errorf("unknown %s %.*q", field, maxEchoLen, value)
}
