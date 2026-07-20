package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The service worker's pushsubscriptionchange handler must send the
// double-submit X-CSRF-Token header on its resubscription POST, but a service
// worker cannot read document.cookie — so it fetches the token here with its
// session cookie instead.
func TestCSRFTokenEndpointReturnsSessionToken(t *testing.T) {
	srv := newTestServer(t)
	all, err := srv.users.List()
	if err != nil || len(all) == 0 {
		t.Fatalf("no test user available: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/csrf", nil)
	authRequestAs(srv, req, all[0].ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "csrf-token-" + all[0].ID; resp.CSRFToken != want {
		t.Fatalf("csrfToken = %q, want the token paired with the caller's session (%q)", resp.CSRFToken, want)
	}
}

func TestCSRFTokenEndpointRequiresSession(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/csrf", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
