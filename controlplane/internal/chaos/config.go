// Package chaos keeps the one-command demo busy: it registers the simulated
// fleet, books reservations, plants implants and GPU faults, and repairs what
// the platform quarantined. It is an external client of the control plane,
// bmcsim and the GPU agents. See docs/demo.md.
package chaos

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	envControlPlaneURL = "CHAOS_CONTROLPLANE_URL"
	envBMCSimURL       = "CHAOS_BMCSIM_URL"
	envNodeBMCURL      = "CHAOS_NODE_BMC_URL"
	envBMCUser         = "BMCSIM_USER"
	envBMCPassword     = "BMCSIM_PASSWORD"
	envAgents          = "CHAOS_AGENTS"
	envSeed            = "CHAOS_SEED"
	envTick            = "CHAOS_TICK"

	defaultControlPlaneURL = "http://controlplane:8080"
	defaultBMCSimURL       = "http://bmcsim:8443"
	defaultAgents          = "node-001=http://agent-node-001:9400,node-002=http://agent-node-002:9400," +
		"node-003=http://agent-node-003:9400,node-004=http://agent-node-004:9400"
	defaultSeed uint64 = 1
	defaultTick        = 10 * time.Second
)

var validNodeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Config is the chaos service's environment. Agents maps a node ID to the
// base URL of the GPU agent running on that node.
type Config struct {
	ControlPlaneURL string
	BMCSimURL       string
	NodeBMCURL      string
	BMCUser         string
	BMCPassword     string
	Agents          map[string]string
	Seed            uint64
	Tick            time.Duration
}

// LoadConfig reads and validates the configuration, reporting every problem
// at once. An unset or blank variable takes its default.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	l := &loader{lookup: lookup}
	cfg := Config{
		ControlPlaneURL: l.url(envControlPlaneURL, defaultControlPlaneURL),
		BMCSimURL:       l.url(envBMCSimURL, defaultBMCSimURL),
	}
	cfg.NodeBMCURL = l.url(envNodeBMCURL, cfg.BMCSimURL)
	cfg.BMCUser = l.required(envBMCUser)
	cfg.BMCPassword = l.required(envBMCPassword)
	cfg.Agents = l.agents(envAgents, defaultAgents)
	cfg.Seed = l.seed(envSeed, defaultSeed)
	cfg.Tick = l.duration(envTick, defaultTick)
	if err := errors.Join(l.errs...); err != nil {
		return Config{}, fmt.Errorf("chaos config: %w", err)
	}
	return cfg, nil
}

type loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (l *loader) fail(key, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s %s", key, fmt.Sprintf(format, args...)))
}

func (l *loader) value(key, def string) string {
	if v, ok := l.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (l *loader) required(key string) string {
	v := l.value(key, "")
	if v == "" {
		l.fail(key, "is required")
	}
	return v
}

// url returns "" for a bad value; the URL itself is never echoed because a
// misplaced credential must not reach the logs.
func (l *loader) url(key, def string) string {
	raw := l.value(key, def)
	if raw == "" {
		return ""
	}
	base, ok := parseBaseURL(raw)
	if !ok {
		l.fail(key, "must be an absolute http or https URL with a host and no credentials, query or fragment")
	}
	return base
}

func (l *loader) agents(key, def string) map[string]string {
	agents, err := parseAgents(l.value(key, def))
	if err != nil {
		l.fail(key, "is invalid: %v", err)
	}
	return agents
}

func (l *loader) seed(key string, def uint64) uint64 {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	seed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		l.fail(key, "must be an unsigned integer, got %q", raw)
	}
	return seed
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		l.fail(key, "must be a positive duration such as 10s, got %q", raw)
	}
	return d
}

func parseBaseURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || strings.ContainsAny(raw, "?#") || u.User != nil || u.Hostname() == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return strings.TrimRight(raw, "/"), true
}

func parseAgents(raw string) (map[string]string, error) {
	agents := map[string]string{}
	var errs []error
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, base, err := parseAgentPair(pair)
		if err == nil {
			if _, dup := agents[id]; dup {
				err = fmt.Errorf("node %q listed twice", id)
			}
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		agents[id] = base
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return agents, nil
}

func parseAgentPair(pair string) (id, base string, err error) {
	id, rawURL, ok := strings.Cut(pair, "=")
	id, rawURL = strings.TrimSpace(id), strings.TrimSpace(rawURL)
	if !ok || id == "" || rawURL == "" {
		return "", "", fmt.Errorf("entry %q must be node-id=url", pair)
	}
	if !validNodeID.MatchString(id) {
		return "", "", fmt.Errorf("node id %q is not a valid ID", id)
	}
	base, ok = parseBaseURL(rawURL)
	if !ok {
		return "", "", fmt.Errorf("agent URL for %q must be an absolute http or https URL", id)
	}
	return id, base, nil
}
