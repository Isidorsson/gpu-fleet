// Package redfish is a client for the Redfish subset in docs/bmc-protocol.md.
package redfish

import "errors"

type ResetType string

const (
	ResetOn           ResetType = "On"
	ResetForceOff     ResetType = "ForceOff"
	ResetForceRestart ResetType = "ForceRestart"
)

type PowerState string

const (
	PowerOn  PowerState = "On"
	PowerOff PowerState = "Off"
)

type Health string

const (
	HealthOK       Health = "OK"
	HealthCritical Health = "Critical"
)

type System struct {
	ID         string
	PowerState PowerState
	Health     Health
	// PXEOnce is true when the next boot will PXE boot once.
	PXEOnce bool
}

var (
	ErrNotFound     = errors.New("redfish: system not found")
	ErrUnauthorized = errors.New("redfish: unauthorized")
	// ErrTransient marks failures worth retrying: 503s, timeouts, network errors.
	ErrTransient = errors.New("redfish: transient failure")
)
