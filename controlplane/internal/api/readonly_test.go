package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadOnlyAllowsReads(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		ReadOnly(inner).ServeHTTP(rec, httptest.NewRequest(method, "/v1/nodes", nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s reached handler = %d, want passthrough", method, rec.Code)
		}
	}
}

func TestReadOnlyBlocksWrites(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		ReadOnly(inner).ServeHTTP(rec, httptest.NewRequest(method, "/v1/alerts", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s = %d allow=%q, want 405 with Allow", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if called {
		t.Fatal("a write reached the wrapped handler")
	}
}
