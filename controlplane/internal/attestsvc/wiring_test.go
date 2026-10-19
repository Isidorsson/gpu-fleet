package attestsvc

import (
	"gpu-fleet/controlplane/internal/api"
	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/store"
)

// These fail to compile if the real implementations drift from the interfaces
// main wires them into.
var (
	_ Store            = (*store.Postgres)(nil)
	_ Agent            = (*deployagent.Client)(nil)
	_ api.Attestations = (*Service)(nil)
	_ Verify           = attest.Verify
)
