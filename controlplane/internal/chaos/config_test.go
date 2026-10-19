package chaos

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

func envOf(overrides map[string]string) func(string) (string, bool) {
	base := map[string]string{envBMCUser: "sim", envBMCPassword: "sim-dev-only"}
	maps.Copy(base, overrides)
	return func(key string) (string, bool) {
		v, ok := base[key]
		return v, ok
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ControlPlaneURL: "http://controlplane:8080",
		BMCSimURL:       "http://bmcsim:8443",
		NodeBMCURL:      "http://bmcsim:8443",
		BMCUser:         "sim",
		BMCPassword:     "sim-dev-only",
		Agents: map[string]string{
			"node-001": "http://agent-node-001:9400",
			"node-002": "http://agent-node-002:9400",
			"node-003": "http://agent-node-003:9400",
			"node-004": "http://agent-node-004:9400",
		},
		Seed: 1,
		Tick: 10 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{
		envControlPlaneURL: "http://localhost:8080/",
		envBMCSimURL:       "https://bmc.example:9443",
		envAgents:          " node-001 = http://a:1 , node-009=http://b:2/ ,",
		envSeed:            "42",
		envTick:            "250ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != "http://localhost:8080" {
		t.Errorf("control plane URL = %q, want the trailing slash trimmed", cfg.ControlPlaneURL)
	}
	if cfg.BMCSimURL != "https://bmc.example:9443" || cfg.NodeBMCURL != cfg.BMCSimURL {
		t.Errorf("node BMC URL must follow CHAOS_BMCSIM_URL by default, got %q and %q", cfg.BMCSimURL, cfg.NodeBMCURL)
	}
	if want := map[string]string{"node-001": "http://a:1", "node-009": "http://b:2"}; !reflect.DeepEqual(cfg.Agents, want) {
		t.Errorf("agents = %v, want %v", cfg.Agents, want)
	}
	if cfg.Seed != 42 || cfg.Tick != 250*time.Millisecond {
		t.Errorf("seed %d tick %s", cfg.Seed, cfg.Tick)
	}
}

func TestLoadConfigNodeBMCURLIsIndependent(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{envNodeBMCURL: "http://10.0.0.5:8443"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeBMCURL != "http://10.0.0.5:8443" || cfg.BMCSimURL != "http://bmcsim:8443" {
		t.Errorf("node BMC %q, bmcsim %q", cfg.NodeBMCURL, cfg.BMCSimURL)
	}
}

func TestLoadConfigBlankVariablesTakeTheDefault(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{envTick: "  ", envSeed: "", envAgents: "", envBMCSimURL: ""}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tick != defaultTick || cfg.Seed != 1 || len(cfg.Agents) != 4 || cfg.BMCSimURL != defaultBMCSimURL {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing user", map[string]string{envBMCUser: ""}, "BMCSIM_USER is required"},
		{"missing password", map[string]string{envBMCPassword: " "}, "BMCSIM_PASSWORD is required"},
		{"tick not a duration", map[string]string{envTick: "soon"}, "CHAOS_TICK must be a positive duration"},
		{"tick zero", map[string]string{envTick: "0s"}, "CHAOS_TICK must be a positive duration"},
		{"tick negative", map[string]string{envTick: "-5s"}, "CHAOS_TICK must be a positive duration"},
		{"tick without unit", map[string]string{envTick: "10"}, "CHAOS_TICK must be a positive duration"},
		{"seed not a number", map[string]string{envSeed: "abc"}, "CHAOS_SEED must be an unsigned integer"},
		{"seed negative", map[string]string{envSeed: "-1"}, "CHAOS_SEED must be an unsigned integer"},
		{"seed fractional", map[string]string{envSeed: "1.5"}, "CHAOS_SEED must be an unsigned integer"},
		{"control plane without scheme", map[string]string{envControlPlaneURL: "controlplane:8080"}, "CHAOS_CONTROLPLANE_URL must be an absolute"},
		{"control plane wrong scheme", map[string]string{envControlPlaneURL: "ftp://x:21"}, "CHAOS_CONTROLPLANE_URL must be an absolute"},
		{"bmcsim without host", map[string]string{envBMCSimURL: "http://"}, "CHAOS_BMCSIM_URL must be an absolute"},
		{"node bmc with credentials", map[string]string{envNodeBMCURL: "http://u:p@bmc:8443"}, "CHAOS_NODE_BMC_URL must be an absolute"},
		{"node bmc with query", map[string]string{envNodeBMCURL: "http://bmc:8443/?x=1"}, "CHAOS_NODE_BMC_URL must be an absolute"},
		{"node bmc with fragment", map[string]string{envNodeBMCURL: "http://bmc:8443/#x"}, "CHAOS_NODE_BMC_URL must be an absolute"},
		{"agent without equals", map[string]string{envAgents: "node-001"}, `entry "node-001" must be node-id=url`},
		{"agent without url", map[string]string{envAgents: "node-001="}, "must be node-id=url"},
		{"agent without id", map[string]string{envAgents: "=http://a:1"}, "must be node-id=url"},
		{"agent with bad id", map[string]string{envAgents: "no de=http://a:1"}, "is not a valid ID"},
		{"agent with bad url", map[string]string{envAgents: "node-001=agent:9400"}, `agent URL for "node-001"`},
		{"agent listed twice", map[string]string{envAgents: "node-001=http://a:1,node-001=http://b:1"}, `node "node-001" listed twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(envOf(tt.env))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	_, err := LoadConfig(envOf(map[string]string{
		envBMCUser: "", envBMCPassword: "", envTick: "x", envSeed: "y", envAgents: "bad",
	}))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"BMCSIM_USER", "BMCSIM_PASSWORD", "CHAOS_TICK", "CHAOS_SEED", "CHAOS_AGENTS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadConfigNeverEchoesCredentialsOrURLs(t *testing.T) {
	_, err := LoadConfig(envOf(map[string]string{
		envNodeBMCURL: "http://admin:hunter2@bmc:8443",
		envBMCUser:    "",
	}))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "sim-dev-only") {
		t.Errorf("error leaks a secret: %v", err)
	}
}
