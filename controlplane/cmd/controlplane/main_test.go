package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func marker(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", name)
	})
}

func TestNewHandlerRoutesMetricsAndAPI(t *testing.T) {
	h := newHandler(marker("api"), marker("metrics"))
	tests := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/metrics", "metrics"},
		{http.MethodGet, "/healthz", "api"},
		{http.MethodGet, "/v1/nodes", "api"},
		{http.MethodPost, "/v1/nodes", "api"},
		{http.MethodPost, "/metrics", "api"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if got := rec.Header().Get("X-Handler"); got != tc.want {
				t.Fatalf("served by %q, want %q", got, tc.want)
			}
		})
	}
}
