package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIDocsRouteIsOptIn(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/docs/", nil)

	disabled := httptest.NewRecorder()
	New(nil, nil, nil).Handler().ServeHTTP(disabled, request)
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled docs status = %d, want 404", disabled.Code)
	}

	enabled := httptest.NewRecorder()
	docs := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	New(nil, nil, nil, WithAPIDocs(docs)).Handler().ServeHTTP(enabled, request)
	if enabled.Code != http.StatusOK {
		t.Fatalf("enabled docs status = %d, want 200", enabled.Code)
	}
}
