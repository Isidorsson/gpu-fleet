package chaos

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// rngStream is the second word of the PCG seed; the first is CHAOS_SEED.
const rngStream = 0x6368616f73 // "chaos"

// Service ties the pieces together: startup, then one snapshot, plan and
// action per tick. It is not safe for concurrent use.
type Service struct {
	cfg         Config
	cp          *ControlPlane
	bmc         *BMC
	exec        *Executor
	agentBacked map[string]bool
	rng         *rand.Rand
	state       State
	backoff     Backoff
	log         *slog.Logger
	now         func() time.Time
}

func New(cfg Config, log *slog.Logger) *Service {
	cp := NewControlPlane(cfg.ControlPlaneURL)
	bmc := NewBMC(cfg.BMCSimURL, cfg.BMCUser, cfg.BMCPassword)
	agents := make(map[string]*Agent, len(cfg.Agents))
	agentBacked := make(map[string]bool, len(cfg.Agents))
	for id, base := range cfg.Agents {
		agents[id] = NewAgent(base)
		agentBacked[id] = true
	}
	return &Service{
		cfg:         cfg,
		cp:          cp,
		bmc:         bmc,
		exec:        NewExecutor(cp, bmc, agents),
		agentBacked: agentBacked,
		rng:         rand.New(rand.NewPCG(cfg.Seed, rngStream)),
		backoff:     defaultBackoff,
		log:         log,
		now:         time.Now,
	}
}

// Run starts up and then ticks until ctx is done. A tick that fails is
// logged and the next one tries again; only startup can fail Run.
func (s *Service) Run(ctx context.Context) error {
	if err := s.Startup(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick takes a snapshot, plans at most one action, executes it and logs one
// line.
func (s *Service) Tick(ctx context.Context) {
	snap, err := takeSnapshot(ctx, s.cp, s.agentBacked)
	if err != nil {
		s.log.Warn("snapshot failed, skipping tick", "err", err)
		return
	}
	now := s.now()
	s.state = s.state.Observe(snap, now)
	action, ok := Plan(snap, s.state, now, s.rng)
	if !ok {
		return
	}
	outcome, err := s.exec.Execute(ctx, action)
	s.state = s.state.Attempted(action, now)
	if err != nil {
		s.log.Warn("action failed", "kind", action.Kind, "action", action.String(), "err", err)
		return
	}
	s.state = s.state.Applied(action, now)
	s.log.Info("action", "kind", action.Kind, "action", action.String(), "outcome", outcome)
}
