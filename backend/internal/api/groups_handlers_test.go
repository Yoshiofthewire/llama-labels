package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGroupsGetAcceptsSubscriberHash drives the endpoint through the
// server's real route table (not a hand-wired middleware call) so it fails
// if GET /api/groups is ever wired back to withAuth instead of
// withMailAuth. Mobile clients only have subscriberId/subscriberHash
// pairing, never a session cookie — see Client_Contact_Update.md Part 0.
func TestGroupsGetAcceptsSubscriberHash(t *testing.T) {
	srv := newTestServer(t)
	store := testUserStore(t, srv)
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}
	hash := srv.pairingSubscriberHash(subscriberID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/groups?sub="+subscriberID+"&hash="+hash, nil)
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (pairing auth should reach the handler); body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
