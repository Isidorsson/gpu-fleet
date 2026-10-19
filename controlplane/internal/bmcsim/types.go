package bmcsim

import (
	"errors"
	"fmt"
)

var ErrInvalid = errors.New("invalid value")

type Power string

const (
	PowerOn  Power = "On"
	PowerOff Power = "Off"
)

type Health string

const (
	HealthOK       Health = "OK"
	HealthCritical Health = "Critical"
)

type BootTarget string

const (
	BootNone BootTarget = "None"
	BootPxe  BootTarget = "Pxe"
)

type BootEnabled string

const (
	BootDisabled BootEnabled = "Disabled"
	BootOnce     BootEnabled = "Once"
)

type ResetType string

const (
	ResetOn           ResetType = "On"
	ResetForceOff     ResetType = "ForceOff"
	ResetForceRestart ResetType = "ForceRestart"
)

type Task string

const (
	TaskInspect Task = "inspect"
	TaskWipe    Task = "wipe"
	TaskDeploy  Task = "deploy"
)

type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
)

type BootOverride struct {
	Target  BootTarget
	Enabled BootEnabled
}

// BootPatch carries only the fields a PATCH set; nil fields stay unchanged.
type BootPatch struct {
	Target  *BootTarget
	Enabled *BootEnabled
}

func ParseBootTarget(raw string) (BootTarget, error) {
	return parseEnum("BootSourceOverrideTarget", raw, BootNone, BootPxe)
}

func ParseBootEnabled(raw string) (BootEnabled, error) {
	return parseEnum("BootSourceOverrideEnabled", raw, BootDisabled, BootOnce)
}

func ParseResetType(raw string) (ResetType, error) {
	return parseEnum("ResetType", raw, ResetOn, ResetForceOff, ResetForceRestart)
}

func ParseTask(raw string) (Task, error) {
	return parseEnum("task", raw, TaskInspect, TaskWipe, TaskDeploy)
}

func parseEnum[T ~string](kind, raw string, allowed ...T) (T, error) {
	for _, a := range allowed {
		if string(a) == raw {
			return a, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("%w: unknown %s %q", ErrInvalid, kind, raw)
}
