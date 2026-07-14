package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llama-lab/backend/internal/contacts"
	"llama-lab/backend/internal/pgpmail"
)

func TestPGPKeyserverLookupSuccess(t *testing.T) {
	id, err := pgpmail.GenerateIdentity("Keyserver Test", "keyserver-test@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vks/v1/by-email/keyserver-test@example.com" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/pgp-keys")
		_, _ = w.Write([]byte(id.ArmoredPublicKey))
	}))
	defer ts.Close()

	original := keyserverBaseURL
	keyserverBaseURL = ts.URL
	defer func() { keyserverBaseURL = original }()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/pgp/keyserver/lookup?email=keyserver-test@example.com", nil)
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPKeyserverLookup)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: got %s want %s", resp.Fingerprint, id.Fingerprint)
	}
}

func TestPGPKeyserverLookupNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	original := keyserverBaseURL
	keyserverBaseURL = ts.URL
	defer func() { keyserverBaseURL = original }()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/pgp/keyserver/lookup?email=nobody@example.com", nil)
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPKeyserverLookup)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestPGPRecipientsCheck(t *testing.T) {
	srv := newTestServer(t)
	all, _ := srv.users.List()
	userID := all[0].ID
	contactsStore, err := srv.userContactsStore(userID)
	if err != nil {
		t.Fatalf("userContactsStore: %v", err)
	}
	if _, err := contactsStore.Upsert(contacts.Contact{
		FormattedName: "Has Key",
		Emails:        []contacts.ContactValue{{Value: "haskey@example.com"}},
		PGPKey:        "-----BEGIN PGP PUBLIC KEY BLOCK-----\n...\n-----END PGP PUBLIC KEY BLOCK-----",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"addresses": []string{"haskey@example.com", "nokey@example.com"}})
	req := httptest.NewRequest(http.MethodPost, "/api/pgp/recipients/check", bytes.NewReader(body))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPRecipientsCheck)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []struct {
			Address string `json:"address"`
			HasKey  bool   `json:"hasKey"`
		} `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 2 || !resp.Results[0].HasKey || resp.Results[1].HasKey {
		t.Fatalf("unexpected results: %+v", resp.Results)
	}
}
