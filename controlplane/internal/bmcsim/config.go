package bmcsim

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrInvalidConfig = errors.New("invalid config")

type Config struct {
	Nodes        int
	Seed         uint64
	BootLatency  time.Duration
	TaskDuration time.Duration
	InspectFail  float64
	WipeFail     float64
	DeployFail   float64
	BMCErrorRate float64
	User         string
	Password     string
}

func (c Config) Validate() error {
	if c.Nodes <= 0 {
		return fmt.Errorf("%w: nodes must be positive, got %d", ErrInvalidConfig, c.Nodes)
	}
	if c.User == "" || c.Password == "" {
		return fmt.Errorf("%w: user and password are required", ErrInvalidConfig)
	}
	if c.BootLatency < 0 || c.TaskDuration < 0 {
		return fmt.Errorf("%w: durations must not be negative", ErrInvalidConfig)
	}
	probabilities := []struct {
		name  string
		value float64
	}{
		{"inspect failure", c.InspectFail},
		{"wipe failure", c.WipeFail},
		{"deploy failure", c.DeployFail},
		{"BMC error rate", c.BMCErrorRate},
	}
	for _, p := range probabilities {
		if math.IsNaN(p.value) || p.value < 0 || p.value > 1 {
			return fmt.Errorf("%w: %s must be within [0,1], got %v", ErrInvalidConfig, p.name, p.value)
		}
	}
	return nil
}

func (c Config) params() Params {
	return Params{
		BootLatency:  c.BootLatency,
		TaskDuration: c.TaskDuration,
		FailProb: map[Task]float64{
			TaskInspect: c.InspectFail,
			TaskWipe:    c.WipeFail,
			TaskDeploy:  c.DeployFail,
		},
	}
}
