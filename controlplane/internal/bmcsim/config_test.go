package bmcsim

import (
	"errors"
	"math"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{Nodes: 1, User: "u", Password: "p"}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		valid  bool
	}{
		{"minimal", func(*Config) {}, true},
		{"boundary probabilities", func(c *Config) { c.InspectFail = 1; c.WipeFail = 0; c.BMCErrorRate = 1 }, true},
		{"zero nodes", func(c *Config) { c.Nodes = 0 }, false},
		{"negative nodes", func(c *Config) { c.Nodes = -1 }, false},
		{"empty user", func(c *Config) { c.User = "" }, false},
		{"empty password", func(c *Config) { c.Password = "" }, false},
		{"negative boot latency", func(c *Config) { c.BootLatency = -time.Second }, false},
		{"negative task duration", func(c *Config) { c.TaskDuration = -time.Second }, false},
		{"inspect above one", func(c *Config) { c.InspectFail = 1.01 }, false},
		{"wipe negative", func(c *Config) { c.WipeFail = -0.1 }, false},
		{"deploy NaN", func(c *Config) { c.DeployFail = math.NaN() }, false},
		{"error rate above one", func(c *Config) { c.BMCErrorRate = 2 }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
			if !tc.valid && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewHandlerRejectsInvalidConfig(t *testing.T) {
	cfg := validConfig()
	cfg.Nodes = 0
	if _, err := NewHandler(cfg, SystemClock{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewHandler = %v, want ErrInvalidConfig", err)
	}
}

func TestNodeIDs(t *testing.T) {
	f, err := NewFleet(Config{Nodes: 3, User: "u", Password: "p"}, SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	ids := f.IDs()
	want := []string{"node-001", "node-002", "node-003"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("IDs = %v, want %v", ids, want)
		}
	}
	if _, ok := f.Machine("node-004"); ok {
		t.Fatal("node-004 should not exist")
	}
}
