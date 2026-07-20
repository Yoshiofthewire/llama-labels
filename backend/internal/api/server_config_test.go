package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kypost-server/backend/internal/config"
	"kypost-server/backend/internal/users"
)

// TestConfigPutRequiresAdmin is a regression test: PUT /api/config used to be
// reachable by any authenticated user, not just admins, letting a non-admin
// account overwrite install-wide settings (redaction patterns, rate limits,
// label allowlist) that only the Classifier sub-struct was ever meant to gate.
func TestConfigPutRequiresAdmin(t *testing.T) {
	srv := newTestServer(t)
	admin, regular := newTestUsers(t, srv)
	srv.configPath = t.TempDir() + "/config.yaml"

	srv.mu.Lock()
	originalPatternCount := len(srv.cfg.Redaction.Patterns)
	srv.mu.Unlock()
	if originalPatternCount == 0 {
		t.Fatal("expected the default config to seed at least one redaction pattern")
	}

	next := config.Default()
	next.Redaction.Patterns = nil // what a malicious/careless non-admin PUT would try to do
	body, _ := json.Marshal(next)

	// Non-admin PUT is rejected outright, before ever reaching handleConfig.
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	authRequestAs(srv, req, regular.ID)
	rec := httptest.NewRecorder()
	srv.withAdmin(srv.handleConfig)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT /api/config: status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	srv.mu.Lock()
	stillIntact := len(srv.cfg.Redaction.Patterns)
	srv.mu.Unlock()
	if stillIntact != originalPatternCount {
		t.Fatalf("redaction patterns were modified by a rejected non-admin PUT: got %d, want %d", stillIntact, originalPatternCount)
	}

	// The same payload from an admin is accepted.
	req = httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	authRequestAs(srv, req, admin.ID)
	rec = httptest.NewRecorder()
	srv.withAdmin(srv.handleConfig)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT /api/config: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// TestConfigGetMasksLLMAPIKeyForNonAdmin is a regression test: the remote-LLM
// API key is admin-only to edit (the frontend hides the "llm" tab from
// non-admins), but GET /api/config previously returned it in plaintext to
// any authenticated session regardless of role.
func TestConfigGetMasksLLMAPIKeyForNonAdmin(t *testing.T) {
	srv := newTestServer(t)
	admin, regular := newTestUsers(t, srv)

	srv.mu.Lock()
	srv.cfg.Classifier.APIKey = "sk-super-secret-key"
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	authRequestAs(srv, req, regular.ID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-admin GET /api/config: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var nonAdminCfg config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &nonAdminCfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if nonAdminCfg.Classifier.APIKey != "" {
		t.Fatalf("non-admin GET /api/config leaked the LLM API key: %q", nonAdminCfg.Classifier.APIKey)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/config", nil)
	authRequestAs(srv, req, admin.ID)
	rec = httptest.NewRecorder()
	srv.withAuth(srv.handleConfig)(rec, req)
	var adminCfg config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &adminCfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if adminCfg.Classifier.APIKey != "sk-super-secret-key" {
		t.Fatalf("admin GET /api/config APIKey = %q, want the real key", adminCfg.Classifier.APIKey)
	}
}

// TestChangePasswordRevokesOtherSessions is a regression test: changing a
// password used to leave every other live session for the account (e.g. a
// stolen cookie) valid for up to the remaining 24h sliding-expiry window.
// The session that performs the change itself must stay logged in.
func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("heidi", "old-password", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	changingToken := "changing-session-token"
	otherToken := "other-stolen-session-token"
	srv.mu.Lock()
	srv.sessions[changingToken] = Session{UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour), CSRFToken: "csrf-a"}
	srv.sessions[otherToken] = Session{UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour), CSRFToken: "csrf-b"}
	srv.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"oldPassword": "old-password", "newPassword": "new-password"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: changingToken})
	req.Header.Set("X-CSRF-Token", "csrf-a")
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleChangePassword)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	srv.mu.Lock()
	_, changingStillLive := srv.sessions[changingToken]
	_, otherStillLive := srv.sessions[otherToken]
	srv.mu.Unlock()
	if !changingStillLive {
		t.Error("the session that performed the password change was itself revoked; it should stay logged in")
	}
	if otherStillLive {
		t.Error("a different live session for the same account survived a password change; it should have been revoked")
	}
}

// TestAdminResetPasswordRevokesTargetSessions mirrors
// TestChangePasswordRevokesOtherSessions for the admin-triggered path: none
// of the target account's sessions belong to the admin, so all of them (not
// "all but one") must be revoked.
func TestAdminResetPasswordRevokesTargetSessions(t *testing.T) {
	srv := newTestServer(t)
	admin, target := newTestUsers(t, srv)

	targetToken := "target-session-token"
	srv.mu.Lock()
	srv.sessions[targetToken] = Session{UserID: target.ID, ExpiresAt: time.Now().Add(24 * time.Hour), CSRFToken: "csrf-target"}
	srv.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"password": "brand-new-password"})
	req := httptest.NewRequest(http.MethodPost, "/api/users/"+target.ID+"/reset-password", bytes.NewReader(body))
	req.SetPathValue("id", target.ID)
	authRequestAs(srv, req, admin.ID)
	rec := httptest.NewRecorder()
	srv.withAdmin(srv.handleUsersResetPassword)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reset password: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	srv.mu.Lock()
	_, targetStillLive := srv.sessions[targetToken]
	srv.mu.Unlock()
	if targetStillLive {
		t.Error("target's session survived an admin password reset; it should have been revoked")
	}
}
