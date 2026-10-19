// Package deployagent is a client for the in-band deploy agent described in
// docs/bmc-protocol.md.
package deployagent

import "errors"

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

type Status struct {
	Task  Task
	Phase Phase
}

var (
	ErrNotFound     = errors.New("deployagent: system not found")
	ErrUnauthorized = errors.New("deployagent: unauthorized")
	ErrTransient    = errors.New("deployagent: transient failure")
)
