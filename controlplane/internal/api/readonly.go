package api

import "net/http"

// ReadOnly wraps h so only GET and HEAD reach it. It backs the public
// listener of a showcase deployment: the full API stays on a private
// listener, so the public side cannot reach any write path at all.
func ReadOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
				"error": "this is a read-only public endpoint",
			})
			return
		}
		h.ServeHTTP(w, r)
	})
}
