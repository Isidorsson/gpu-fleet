package bmcsim

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
)

const (
	maxBodyBytes = 1 << 16
	authRealm    = `Basic realm="bmcsim"`
	// injectorStream keeps the error-injection RNG off every machine's stream.
	injectorStream = ^uint64(0)
)

type server struct {
	fleet  *Fleet
	creds  credentials
	faults *injector
}

type machineFunc func(w http.ResponseWriter, r *http.Request, m *Machine)

func NewHandler(cfg Config, clock Clock) (http.Handler, error) {
	s, err := newServer(cfg, clock)
	if err != nil {
		return nil, err
	}
	return s.routes(), nil
}

func newServer(cfg Config, clock Clock) (*server, error) {
	fleet, err := NewFleet(cfg, clock)
	if err != nil {
		return nil, fmt.Errorf("build fleet: %w", err)
	}
	return &server{
		fleet:  fleet,
		creds:  newCredentials(cfg.User, cfg.Password),
		faults: newInjector(cfg.Seed, cfg.BMCErrorRate),
	}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /redfish/v1/Systems", s.listSystems)
	mux.HandleFunc("GET /redfish/v1/Systems/{id}", s.machineRoute(s.getSystem))
	mux.HandleFunc("PATCH /redfish/v1/Systems/{id}", s.machineRoute(s.patchSystem))
	mux.HandleFunc("POST /redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset", s.machineRoute(s.resetSystem))
	mux.HandleFunc("PUT /agent/{id}/task", s.machineRoute(s.putTask))
	mux.HandleFunc("GET /agent/{id}/status", s.machineRoute(s.getStatus))
	mux.HandleFunc("GET /agent/{id}/identity", s.machineRoute(s.getIdentity))
	mux.HandleFunc("GET /agent/{id}/attestation", s.machineRoute(s.getAttestation))
	mux.HandleFunc("POST /sim/systems/{id}/fault", s.simRoute(setHealth(HealthCritical)))
	mux.HandleFunc("DELETE /sim/systems/{id}/fault", s.simRoute(setHealth(HealthOK)))
	mux.HandleFunc("POST /sim/systems/{id}/tamper", s.simRoute(s.postTamper))
	mux.HandleFunc("DELETE /sim/systems/{id}/tamper", s.simRoute(clearTamper))
	mux.HandleFunc("GET /sim/vendor-root", s.getVendorRoot)
	return s.authenticate(mux)
}

func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !s.creds.match(user, pass) {
			w.Header().Set("WWW-Authenticate", authRealm)
			writeStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// machineRoute resolves the system (404) and then applies error injection
// (503), so unknown systems answer deterministically.
func (s *server) machineRoute(h machineFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.resolve(w, r)
		if !ok || s.injectFailure(w) {
			return
		}
		h(w, r, m)
	}
}

func (s *server) simRoute(h machineFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.resolve(w, r)
		if !ok {
			return
		}
		h(w, r, m)
	}
}

func (s *server) resolve(w http.ResponseWriter, r *http.Request) (*Machine, bool) {
	m, ok := s.fleet.Machine(r.PathValue("id"))
	if !ok {
		writeStatus(w, http.StatusNotFound, "unknown system")
	}
	return m, ok
}

func (s *server) injectFailure(w http.ResponseWriter) bool {
	if !s.faults.fail() {
		return false
	}
	writeStatus(w, http.StatusServiceUnavailable, "bmc busy")
	return true
}

type credentials struct {
	user, pass [sha256.Size]byte
}

func newCredentials(user, pass string) credentials {
	return credentials{user: sha256.Sum256([]byte(user)), pass: sha256.Sum256([]byte(pass))}
}

// match hashes first so the comparison time reveals neither value nor length.
func (c credentials) match(user, pass string) bool {
	u := sha256.Sum256([]byte(user))
	p := sha256.Sum256([]byte(pass))
	userOK := subtle.ConstantTimeCompare(u[:], c.user[:])
	passOK := subtle.ConstantTimeCompare(p[:], c.pass[:])
	return userOK&passOK == 1
}

type injector struct {
	mu   sync.Mutex
	rng  *rand.Rand
	rate float64
}

func newInjector(seed uint64, rate float64) *injector {
	return &injector{rng: rand.New(rand.NewPCG(seed, injectorStream)), rate: rate}
}

func (i *injector) fail() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.rng.Float64() < i.rate
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed JSON body: %w", ErrInvalid, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after JSON body", ErrInvalid)
	}
	return nil
}

func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalid) {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	writeStatus(w, http.StatusInternalServerError, "internal error")
}

func writeStatus(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers are sent; nothing useful left to do on failure
}

func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
