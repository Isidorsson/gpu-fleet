package bmcsim

import (
	"fmt"
	"net/http"
)

const nonceHexLen = 64

type identityJSON struct {
	AKPub string `json:"ak_pub"`
}

type vendorRootJSON struct {
	VendorRootPub string `json:"vendor_root_pub"`
}

func (s *server) getIdentity(w http.ResponseWriter, r *http.Request, _ *Machine) {
	a := s.fleet.attesters[r.PathValue("id")]
	writeJSON(w, http.StatusOK, identityJSON{AKPub: b64(a.akPub())})
}

// getAttestation validates the nonce before checking power, so a malformed
// request is a 400 whatever state the machine is in.
func (s *server) getAttestation(w http.ResponseWriter, r *http.Request, m *Machine) {
	nonce := r.URL.Query().Get("nonce")
	if !isNonce(nonce) {
		writeError(w, fmt.Errorf("%w: nonce must be %d lowercase hex characters", ErrInvalid, nonceHexLen))
		return
	}
	tampered, powered := m.AttestationState()
	if !powered {
		writeStatus(w, http.StatusConflict, "machine is powered off")
		return
	}
	ev, err := s.fleet.attesters[r.PathValue("id")].evidence(nonce, tampered)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *server) postTamper(w http.ResponseWriter, r *http.Request, m *Machine) {
	var req struct {
		Component string `json:"component"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	component, err := ParseTamper(req.Component)
	if err != nil {
		writeError(w, err)
		return
	}
	m.AddTamper(component)
	writeNoContent(w)
}

func clearTamper(w http.ResponseWriter, _ *http.Request, m *Machine) {
	m.ClearTamper()
	writeNoContent(w)
}

func (s *server) getVendorRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, vendorRootJSON{VendorRootPub: b64(s.fleet.vendorPub)})
}

func isNonce(s string) bool {
	if len(s) != nonceHexLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
