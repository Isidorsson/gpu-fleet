package bmcsim

import (
	"math/rand/v2"
	"sync"
	"time"
)

type Params struct {
	BootLatency  time.Duration
	TaskDuration time.Duration
	FailProb     map[Task]float64
}

type Snapshot struct {
	Power  Power
	Health Health
	Boot   BootOverride
}

type AgentStatus struct {
	Task  Task
	Phase Phase
}

// Machine is one simulated server: BMC state plus the deploy agent that runs
// after a PXE boot. Time only matters through Clock, and a running task is
// resolved lazily by whichever call observes the machine first after it is due.
type Machine struct {
	mu     sync.Mutex
	clock  Clock
	params Params
	rng    *rand.Rand

	power   Power
	health  Health
	boot    BootOverride
	pending Task

	task     Task
	phase    Phase
	doneAt   time.Time
	willFail bool
}

func NewMachine(params Params, clock Clock, rng *rand.Rand) *Machine {
	return &Machine{
		clock:  clock,
		params: params,
		rng:    rng,
		power:  PowerOff,
		health: HealthOK,
		boot:   BootOverride{Target: BootNone, Enabled: BootDisabled},
		phase:  PhaseIdle,
	}
}

func (m *Machine) Snapshot() Snapshot {
	m.begin()
	defer m.mu.Unlock()
	return Snapshot{Power: m.power, Health: m.health, Boot: m.boot}
}

func (m *Machine) Agent() AgentStatus {
	m.begin()
	defer m.mu.Unlock()
	return AgentStatus{Task: m.task, Phase: m.phase}
}

func (m *Machine) PatchBoot(p BootPatch) {
	m.begin()
	defer m.mu.Unlock()
	if p.Target != nil {
		m.boot.Target = *p.Target
	}
	if p.Enabled != nil {
		m.boot.Enabled = *p.Enabled
	}
}

func (m *Machine) SetTask(t Task) {
	m.begin()
	defer m.mu.Unlock()
	m.pending = t
}

func (m *Machine) Reset(rt ResetType) {
	now := m.begin()
	defer m.mu.Unlock()
	switch rt {
	case ResetOn:
		if m.power == PowerOff {
			m.powerOn(now)
		}
	case ResetForceOff:
		m.powerOff()
	case ResetForceRestart:
		m.powerOff()
		m.powerOn(now)
	}
}

// SetHealth with Critical dooms any task still running, even if health is
// restored before the task is due.
func (m *Machine) SetHealth(h Health) {
	m.begin()
	defer m.mu.Unlock()
	m.health = h
	if h == HealthCritical && m.phase == PhaseRunning {
		m.willFail = true
	}
}

// begin locks the machine and settles any task that came due; callers unlock.
func (m *Machine) begin() time.Time {
	m.mu.Lock()
	now := m.clock.Now()
	m.settle(now)
	return now
}

func (m *Machine) settle(now time.Time) {
	if m.phase != PhaseRunning || now.Before(m.doneAt) {
		return
	}
	if m.willFail {
		m.phase = PhaseFailed
		return
	}
	m.phase = PhaseSucceeded
}

func (m *Machine) powerOn(now time.Time) {
	m.power = PowerOn
	m.task, m.phase = "", PhaseIdle
	if m.boot.Target != BootPxe || m.boot.Enabled != BootOnce {
		return
	}
	m.boot = BootOverride{Target: BootNone, Enabled: BootDisabled}
	if m.pending == "" {
		return
	}
	m.startTask(now, m.pending)
	m.pending = ""
}

// startTask always draws from the RNG, so a machine's outcome stream depends
// only on how many tasks it has started, not on health or request timing.
func (m *Machine) startTask(now time.Time, t Task) {
	roll := m.rng.Float64()
	m.task, m.phase = t, PhaseRunning
	m.doneAt = now.Add(m.params.BootLatency + m.params.TaskDuration)
	m.willFail = roll < m.params.FailProb[t] || m.health == HealthCritical
}

func (m *Machine) powerOff() {
	if m.power == PowerOff {
		return
	}
	m.power = PowerOff
	if m.phase == PhaseRunning {
		m.task, m.phase = "", PhaseIdle
	}
}
