package classifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientVersionParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			t.Errorf("unexpected path %q, want /api/version", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"version":"0.32.1"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "", "", "", 0)
	got, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "0.32.1" {
		t.Fatalf("Version = %q, want %q", got, "0.32.1")
	}
}

func TestHTTPClientVersionErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "", "", "", 0)
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("expected an error for a non-2xx /api/version response")
	}
}
