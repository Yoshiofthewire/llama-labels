package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests cover withMailAuth / resolveMailAuthContext: the dual auth
// path (session cookie or mobile subscriberId/subscriberHash) added to the
// mail read/act-on endpoints (inbox, folders, actions, draft, send), plus
// the scope boundary that account setup (/api/imap/config, /api/imap/test)
// stays cookie-only. See Mobile_Mail_Relay.md.

func TestMailAuthCookieStillWorks(t *testing.T) {
	srv := newTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox", nil)
	authRequest(srv, req)
	srv.withMailAuth(srv.handleInbox).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestMailAuthAcceptsSubscriberHash(t *testing.T) {
	srv := newTestServer(t)
	store := testUserStore(t, srv)
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}
	hash := srv.pairingSubscriberHash(subscriberID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox/folders?sub="+subscriberID+"&hash="+hash, nil)
	srv.withMailAuth(srv.handleInboxFolders).ServeHTTP(rec, req)

	// No cookie was set — auth must have succeeded via sub/hash alone for
	// the handler to be reached at all. No IMAP account is configured for
	// this test user, so the handler's own errIMAPNotConfigured path (400)
	// is the expected "auth passed, nothing configured yet" signal.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (imap not configured); body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestMailAuthAcceptsSubscriberHashViaHeader(t *testing.T) {
	srv := newTestServer(t)
	store := testUserStore(t, srv)
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}
	hash := srv.pairingSubscriberHash(subscriberID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox/folders", nil)
	req.Header.Set(headerSubscriberID, subscriberID)
	req.Header.Set(headerSubscriberHash, hash)
	srv.withMailAuth(srv.handleInboxFolders).ServeHTTP(rec, req)

	// No query params and no cookie were set — auth must have succeeded via
	// the headers alone. No IMAP account is configured for this test user,
	// so the handler's own errIMAPNotConfigured path (400) is the expected
	// "auth passed, nothing configured yet" signal — same as the
	// query-param variant above.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (imap not configured); body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestMailAuthRejectsInvalidHash(t *testing.T) {
	srv := newTestServer(t)
	store := testUserStore(t, srv)
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox?sub="+subscriberID+"&hash=deadbeef", nil)
	srv.withMailAuth(srv.handleInbox).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMailAuthRejectsUnknownSubscriber(t *testing.T) {
	srv := newTestServer(t)
	subscriberID := "never-registered"
	hash := srv.pairingSubscriberHash(subscriberID) // a validly-signed hash for an ID no user owns

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox?sub="+subscriberID+"&hash="+hash, nil)
	srv.withMailAuth(srv.handleInbox).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMailAuthNoCredentialsIsPlainUnauthorized(t *testing.T) {
	srv := newTestServer(t)
	// pairingSecret is set (newTestServer default) but no cookie and no
	// sub/hash were supplied at all — this must be an ordinary 401, not a
	// 503, since it looks nothing like a mobile pairing attempt.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox", nil)
	srv.withMailAuth(srv.handleInbox).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMailAuthPairingNotConfigured(t *testing.T) {
	srv := newTestServer(t)
	srv.pairingSecret = ""

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox?sub=someone&hash=anything", nil)
	srv.withMailAuth(srv.handleInbox).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != "pairing is not configured" {
		t.Fatalf("error = %q, want %q", resp.Error, "pairing is not configured")
	}
}

// TestMailAuthScopeBoundaryExcludesAccountSetup confirms /api/imap/config
// and /api/imap/test — wired with withAuth, not withMailAuth — never accept
// subscriberId/subscriberHash. Mobile must never see or set raw mail
// credentials; account setup stays a web-only, cookie-only flow.
func TestMailAuthScopeBoundaryExcludesAccountSetup(t *testing.T) {
	srv := newTestServer(t)
	store := testUserStore(t, srv)
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}
	hash := srv.pairingSubscriberHash(subscriberID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/imap/config?sub="+subscriberID+"&hash="+hash, nil)
	srv.withAuth(srv.handleIMAPConfig).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (withAuth must ignore sub/hash); body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
