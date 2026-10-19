package bmcsim

import (
	"math/rand/v2"
	"testing"
	"time"
)

const (
	testBootLatency  = 3 * time.Second
	testTaskDuration = 5 * time.Second
	testRunTime      = testBootLatency + testTaskDuration
)

var testStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testParams(failProb float64) Params {
	return Params{
		BootLatency:  testBootLatency,
		TaskDuration: testTaskDuration,
		FailProb: map[Task]float64{
			TaskInspect: failProb,
			TaskWipe:    failProb,
			TaskDeploy:  failProb,
		},
	}
}

func newTestMachine(failProb float64) (*Machine, *ManualClock) {
	clock := NewManualClock(testStart)
	return NewMachine(testParams(failProb), clock, rand.New(rand.NewPCG(1, 0))), clock
}

func armPXE(m *Machine, t Task) {
	m.SetTask(t)
	pxe, once := BootPxe, BootOnce
	m.PatchBoot(BootPatch{Target: &pxe, Enabled: &once})
}

func startTask(m *Machine, t Task) {
	armPXE(m, t)
	m.Reset(ResetOn)
}

func wantAgent(t *testing.T, m *Machine, task Task, phase Phase) {
	t.Helper()
	if got := m.Agent(); got != (AgentStatus{Task: task, Phase: phase}) {
		t.Fatalf("agent = %+v, want task %q phase %q", got, task, phase)
	}
}

func TestPXEOnceWithTaskRunsAndConsumesOverride(t *testing.T) {
	m, _ := newTestMachine(0)
	startTask(m, TaskInspect)

	wantAgent(t, m, TaskInspect, PhaseRunning)
	snap := m.Snapshot()
	if snap.Power != PowerOn {
		t.Fatalf("power = %q, want On", snap.Power)
	}
	if want := (BootOverride{Target: BootNone, Enabled: BootDisabled}); snap.Boot != want {
		t.Fatalf("boot = %+v, want %+v", snap.Boot, want)
	}
}

func TestPowerOnWithoutOverrideBootsIdle(t *testing.T) {
	m, _ := newTestMachine(0)
	m.SetTask(TaskInspect)
	m.Reset(ResetOn)

	wantAgent(t, m, "", PhaseIdle)
}

func TestPXEOverrideWithoutTaskIsConsumedAndIdle(t *testing.T) {
	m, _ := newTestMachine(0)
	pxe, once := BootPxe, BootOnce
	m.PatchBoot(BootPatch{Target: &pxe, Enabled: &once})
	m.Reset(ResetOn)

	wantAgent(t, m, "", PhaseIdle)
	if got := m.Snapshot().Boot.Target; got != BootNone {
		t.Fatalf("target = %q, want None", got)
	}
}

func TestTaskIsConsumedByTheBootThatRunsIt(t *testing.T) {
	m, _ := newTestMachine(0)
	startTask(m, TaskWipe)
	pxe, once := BootPxe, BootOnce
	m.PatchBoot(BootPatch{Target: &pxe, Enabled: &once})
	m.Reset(ResetForceRestart)

	wantAgent(t, m, "", PhaseIdle)
}

func TestTaskResolvesAfterBootLatencyPlusDuration(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskDeploy)

	clock.Advance(testRunTime - time.Nanosecond)
	wantAgent(t, m, TaskDeploy, PhaseRunning)

	clock.Advance(time.Nanosecond)
	wantAgent(t, m, TaskDeploy, PhaseSucceeded)
}

func TestOutcomeIsStickyAcrossReads(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testRunTime)
	wantAgent(t, m, TaskInspect, PhaseSucceeded)

	m.SetHealth(HealthCritical)
	clock.Advance(time.Hour)
	wantAgent(t, m, TaskInspect, PhaseSucceeded)
}

func TestFailedOutcomeIsStickyAfterFaultClears(t *testing.T) {
	m, clock := newTestMachine(1)
	startTask(m, TaskInspect)
	clock.Advance(testRunTime)
	wantAgent(t, m, TaskInspect, PhaseFailed)

	m.SetHealth(HealthOK)
	clock.Advance(time.Hour)
	wantAgent(t, m, TaskInspect, PhaseFailed)
}

func TestForceOffAbortsRunningTask(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskWipe)
	clock.Advance(testBootLatency)
	m.Reset(ResetForceOff)

	wantAgent(t, m, "", PhaseIdle)
	if got := m.Snapshot().Power; got != PowerOff {
		t.Fatalf("power = %q, want Off", got)
	}
	clock.Advance(time.Hour)
	wantAgent(t, m, "", PhaseIdle)
}

func TestForceOffAfterDueKeepsFinishedPhaseUntilNextPowerOn(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskWipe)
	clock.Advance(testRunTime)
	m.Reset(ResetForceOff)

	wantAgent(t, m, TaskWipe, PhaseSucceeded)
	clock.Advance(time.Hour)
	wantAgent(t, m, TaskWipe, PhaseSucceeded)

	m.Reset(ResetOn)
	wantAgent(t, m, "", PhaseIdle)
}

func TestOnWhenAlreadyOnIsNoOp(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testBootLatency)

	armPXE(m, TaskWipe)
	m.Reset(ResetOn)

	wantAgent(t, m, TaskInspect, PhaseRunning)
	if got := m.Snapshot().Boot; got != (BootOverride{Target: BootPxe, Enabled: BootOnce}) {
		t.Fatalf("boot = %+v, want override untouched", got)
	}
	clock.Advance(testTaskDuration)
	wantAgent(t, m, TaskInspect, PhaseSucceeded)
}

func TestForceRestartRerunsWithNewTask(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testRunTime)

	armPXE(m, TaskWipe)
	m.Reset(ResetForceRestart)

	wantAgent(t, m, TaskWipe, PhaseRunning)
	clock.Advance(testRunTime - time.Nanosecond)
	wantAgent(t, m, TaskWipe, PhaseRunning)
}

func TestForceRestartFromOffPowersOn(t *testing.T) {
	m, _ := newTestMachine(0)
	armPXE(m, TaskInspect)
	m.Reset(ResetForceRestart)

	if got := m.Snapshot().Power; got != PowerOn {
		t.Fatalf("power = %q, want On", got)
	}
	wantAgent(t, m, TaskInspect, PhaseRunning)
}

func TestForceOffWhenOffIsNoOp(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testRunTime)
	m.Reset(ResetForceOff)
	m.Reset(ResetForceOff)

	wantAgent(t, m, TaskInspect, PhaseSucceeded)
}

func TestCriticalHealthFailsRunningTask(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testBootLatency)
	m.SetHealth(HealthCritical)
	wantAgent(t, m, TaskInspect, PhaseRunning)

	m.SetHealth(HealthOK)
	clock.Advance(testTaskDuration)
	wantAgent(t, m, TaskInspect, PhaseFailed)
}

func TestCriticalHealthAtBootFailsTask(t *testing.T) {
	m, clock := newTestMachine(0)
	m.SetHealth(HealthCritical)
	startTask(m, TaskDeploy)
	clock.Advance(testRunTime)

	wantAgent(t, m, TaskDeploy, PhaseFailed)
}

func TestCriticalHealthAfterTaskDueDoesNotFailIt(t *testing.T) {
	m, clock := newTestMachine(0)
	startTask(m, TaskInspect)
	clock.Advance(testRunTime)
	m.SetHealth(HealthCritical)

	wantAgent(t, m, TaskInspect, PhaseSucceeded)
}

func TestFailureProbabilityBounds(t *testing.T) {
	tests := []struct {
		name string
		prob float64
		want Phase
	}{
		{"zero always succeeds", 0, PhaseSucceeded},
		{"one always fails", 1, PhaseFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, clock := newTestMachine(tc.prob)
			for range 100 {
				startTask(m, TaskInspect)
				clock.Advance(testRunTime)
				wantAgent(t, m, TaskInspect, tc.want)
				m.Reset(ResetForceOff)
			}
		})
	}
}

func TestPerTaskFailureProbability(t *testing.T) {
	clock := NewManualClock(testStart)
	params := testParams(0)
	params.FailProb[TaskWipe] = 1
	m := NewMachine(params, clock, rand.New(rand.NewPCG(1, 0)))

	startTask(m, TaskInspect)
	clock.Advance(testRunTime)
	wantAgent(t, m, TaskInspect, PhaseSucceeded)

	armPXE(m, TaskWipe)
	m.Reset(ResetForceRestart)
	clock.Advance(testRunTime)
	wantAgent(t, m, TaskWipe, PhaseFailed)
}

func TestPatchBootAppliesOnlyGivenFields(t *testing.T) {
	m, _ := newTestMachine(0)
	pxe := BootPxe
	m.PatchBoot(BootPatch{Target: &pxe})

	if got := m.Snapshot().Boot; got != (BootOverride{Target: BootPxe, Enabled: BootDisabled}) {
		t.Fatalf("boot = %+v", got)
	}
}

func TestPXEWithDisabledOverrideBootsFromDisk(t *testing.T) {
	m, _ := newTestMachine(0)
	m.SetTask(TaskInspect)
	pxe := BootPxe
	m.PatchBoot(BootPatch{Target: &pxe})
	m.Reset(ResetOn)

	wantAgent(t, m, "", PhaseIdle)
}

func runOutcomes(f *Fleet, clock *ManualClock, id string, rounds int) []Phase {
	m, _ := f.Machine(id)
	out := make([]Phase, 0, rounds)
	for range rounds {
		startTask(m, TaskInspect)
		clock.Advance(testRunTime)
		out = append(out, m.Agent().Phase)
		m.Reset(ResetForceOff)
	}
	return out
}

func newTestFleet(t *testing.T, seed uint64) (*Fleet, *ManualClock) {
	t.Helper()
	clock := NewManualClock(testStart)
	f, err := NewFleet(Config{
		Nodes: 4, Seed: seed, BootLatency: testBootLatency, TaskDuration: testTaskDuration,
		InspectFail: 0.5, User: "u", Password: "p",
	}, clock)
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	return f, clock
}

func equalPhases(a, b []Phase) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSameSeedGivesSameOutcomes(t *testing.T) {
	const rounds = 64
	f1, c1 := newTestFleet(t, 7)
	f2, c2 := newTestFleet(t, 7)

	got1 := runOutcomes(f1, c1, "node-003", rounds)
	got2 := runOutcomes(f2, c2, "node-003", rounds)
	if !equalPhases(got1, got2) {
		t.Fatalf("same seed diverged:\n%v\n%v", got1, got2)
	}
}

func TestDifferentSeedOrMachineGivesDifferentOutcomes(t *testing.T) {
	const rounds = 64
	f1, c1 := newTestFleet(t, 7)
	f2, c2 := newTestFleet(t, 8)

	base := runOutcomes(f1, c1, "node-001", rounds)
	if equalPhases(base, runOutcomes(f2, c2, "node-001", rounds)) {
		t.Fatal("different seeds produced identical outcomes")
	}
	if equalPhases(base, runOutcomes(f1, c1, "node-002", rounds)) {
		t.Fatal("different machines produced identical outcomes")
	}
}

func TestOutcomesIndependentOfOtherMachinesTraffic(t *testing.T) {
	const rounds = 64
	alone, aloneClock := newTestFleet(t, 9)
	want := runOutcomes(alone, aloneClock, "node-002", rounds)

	busy, busyClock := newTestFleet(t, 9)
	runOutcomes(busy, busyClock, "node-001", rounds/2)
	runOutcomes(busy, busyClock, "node-004", rounds/2)
	got := runOutcomes(busy, busyClock, "node-002", rounds)
	if !equalPhases(want, got) {
		t.Fatalf("outcomes changed with other traffic:\n%v\n%v", want, got)
	}
}

func TestOutcomeStreamIgnoresReadTiming(t *testing.T) {
	const rounds = 32
	f1, c1 := newTestFleet(t, 11)
	want := runOutcomes(f1, c1, "node-001", rounds)

	f2, c2 := newTestFleet(t, 11)
	m, _ := f2.Machine("node-001")
	var got []Phase
	for range rounds {
		startTask(m, TaskInspect)
		for range 4 {
			c2.Advance(testRunTime / 4)
			m.Agent()
		}
		got = append(got, m.Agent().Phase)
		m.Reset(ResetForceOff)
	}
	if !equalPhases(want, got) {
		t.Fatalf("outcomes depend on read timing:\n%v\n%v", want, got)
	}
}

func TestParseEnums(t *testing.T) {
	if got, err := ParseTask("wipe"); err != nil || got != TaskWipe {
		t.Fatalf("ParseTask(wipe) = %q, %v", got, err)
	}
	if got, err := ParseResetType("ForceRestart"); err != nil || got != ResetForceRestart {
		t.Fatalf("ParseResetType = %q, %v", got, err)
	}
	if got, err := ParseBootTarget("Pxe"); err != nil || got != BootPxe {
		t.Fatalf("ParseBootTarget = %q, %v", got, err)
	}
	if got, err := ParseBootEnabled("Once"); err != nil || got != BootOnce {
		t.Fatalf("ParseBootEnabled = %q, %v", got, err)
	}
	for _, bad := range []string{"", "Wipe", "reboot", "on"} {
		if _, err := ParseTask(bad); err == nil {
			t.Errorf("ParseTask(%q) accepted", bad)
		}
	}
	if _, err := ParseResetType("on"); err == nil {
		t.Error("ParseResetType is case sensitive, accepted \"on\"")
	}
}
