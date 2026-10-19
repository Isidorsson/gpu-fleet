package bmcsim

import (
	"fmt"
	"net/http"
)

const (
	systemsPath  = "/redfish/v1/Systems"
	systemStatus = "Enabled"
)

type memberJSON struct {
	ODataID string `json:"@odata.id"`
}

type collectionJSON struct {
	Members []memberJSON `json:"Members"`
	Count   int          `json:"Members@odata.count"`
}

type statusJSON struct {
	State  string `json:"State"`
	Health string `json:"Health"`
}

type bootJSON struct {
	Target  string `json:"BootSourceOverrideTarget"`
	Enabled string `json:"BootSourceOverrideEnabled"`
}

type systemJSON struct {
	ODataID    string     `json:"@odata.id"`
	ID         string     `json:"Id"`
	PowerState string     `json:"PowerState"`
	Status     statusJSON `json:"Status"`
	Boot       bootJSON   `json:"Boot"`
}

type bootPatchJSON struct {
	Target  *string `json:"BootSourceOverrideTarget"`
	Enabled *string `json:"BootSourceOverrideEnabled"`
}

type agentStatusJSON struct {
	Task  string `json:"task"`
	Phase string `json:"phase"`
}

func (s *server) listSystems(w http.ResponseWriter, _ *http.Request) {
	if s.injectFailure(w) {
		return
	}
	ids := s.fleet.IDs()
	out := collectionJSON{Members: make([]memberJSON, 0, len(ids)), Count: len(ids)}
	for _, id := range ids {
		out.Members = append(out.Members, memberJSON{ODataID: systemsPath + "/" + id})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getSystem(w http.ResponseWriter, r *http.Request, m *Machine) {
	id := r.PathValue("id")
	snap := m.Snapshot()
	writeJSON(w, http.StatusOK, systemJSON{
		ODataID:    systemsPath + "/" + id,
		ID:         id,
		PowerState: string(snap.Power),
		Status:     statusJSON{State: systemStatus, Health: string(snap.Health)},
		Boot:       bootJSON{Target: string(snap.Boot.Target), Enabled: string(snap.Boot.Enabled)},
	})
}

func (s *server) patchSystem(w http.ResponseWriter, r *http.Request, m *Machine) {
	var req struct {
		Boot *bootPatchJSON `json:"Boot"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	patch, err := req.Boot.parse()
	if err != nil {
		writeError(w, err)
		return
	}
	m.PatchBoot(patch)
	writeNoContent(w)
}

func (b *bootPatchJSON) parse() (BootPatch, error) {
	var patch BootPatch
	if b == nil {
		return patch, fmt.Errorf("%w: Boot is required", ErrInvalid)
	}
	if b.Target != nil {
		target, err := ParseBootTarget(*b.Target)
		if err != nil {
			return patch, err
		}
		patch.Target = &target
	}
	if b.Enabled != nil {
		enabled, err := ParseBootEnabled(*b.Enabled)
		if err != nil {
			return patch, err
		}
		patch.Enabled = &enabled
	}
	return patch, nil
}

func (s *server) resetSystem(w http.ResponseWriter, r *http.Request, m *Machine) {
	var req struct {
		ResetType string `json:"ResetType"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	rt, err := ParseResetType(req.ResetType)
	if err != nil {
		writeError(w, err)
		return
	}
	m.Reset(rt)
	writeNoContent(w)
}

func (s *server) putTask(w http.ResponseWriter, r *http.Request, m *Machine) {
	var req struct {
		Task string `json:"task"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	task, err := ParseTask(req.Task)
	if err != nil {
		writeError(w, err)
		return
	}
	m.SetTask(task)
	writeNoContent(w)
}

func (s *server) getStatus(w http.ResponseWriter, _ *http.Request, m *Machine) {
	st := m.Agent()
	writeJSON(w, http.StatusOK, agentStatusJSON{Task: string(st.Task), Phase: string(st.Phase)})
}

func setHealth(h Health) machineFunc {
	return func(w http.ResponseWriter, _ *http.Request, m *Machine) {
		m.SetHealth(h)
		writeNoContent(w)
	}
}
