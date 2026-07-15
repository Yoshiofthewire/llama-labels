package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func TestPGPIdentityGenerateThenGetThenDelete(t *testing.T) {
	srv := newTestServer(t)

	genReq := httptest.NewRequest(http.MethodPost, "/api/pgp/identity/generate", nil)
	authRequest(srv, genReq)
	genRec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentityGenerate)(genRec, genReq)
	if genRec.Code != http.StatusOK {
		t.Fatalf("generate: expected 200, got %d: %s", genRec.Code, genRec.Body.String())
	}
	var genResp pgpIdentityResponse
	if err := json.NewDecoder(genRec.Body).Decode(&genResp); err != nil {
		t.Fatalf("decode generate response: %v", err)
	}
	if genResp.Fingerprint == "" || genResp.PublicKey == "" || genResp.Source != "generated" {
		t.Fatalf("unexpected generate response: %+v", genResp)
	}
	if genResp.Revoked || genResp.Expired {
		t.Fatalf("expected a freshly generated identity to be neither revoked nor expired, got %+v", genResp)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/pgp/identity", nil)
	authRequest(srv, getReq)
	getRec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentity)(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var getResp pgpIdentityResponse
	if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if getResp.Fingerprint != genResp.Fingerprint {
		t.Fatalf("fingerprint mismatch: got %s want %s", getResp.Fingerprint, genResp.Fingerprint)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/pgp/identity", nil)
	authRequest(srv, delReq)
	delRec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentity)(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", delRec.Code, delRec.Body.String())
	}

	getReq2 := httptest.NewRequest(http.MethodGet, "/api/pgp/identity", nil)
	authRequest(srv, getReq2)
	getRec2 := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentity)(getRec2, getReq2)
	if getRec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getRec2.Code)
	}
}

func TestPGPIdentityImportWithPassphrase(t *testing.T) {
	srv := newTestServer(t)

	keyGen := crypto.PGP().KeyGeneration().AddUserId("Import Test", "import-test@example.com").New()
	key, err := keyGen.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	locked, err := crypto.PGP().LockKey(key, []byte("s3cret"))
	if err != nil {
		t.Fatalf("LockKey: %v", err)
	}
	armoredLocked, err := locked.Armor()
	if err != nil {
		t.Fatalf("Armor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"armoredPrivateKey": armoredLocked,
		"passphrase":        "s3cret",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/pgp/identity/import", bytes.NewReader(body))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentityImport)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp pgpIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if resp.Fingerprint != key.GetFingerprint() || resp.Source != "imported" {
		t.Fatalf("unexpected import response: %+v", resp)
	}

	badBody, _ := json.Marshal(map[string]string{
		"armoredPrivateKey": armoredLocked,
		"passphrase":        "wrong",
	})
	badReq := httptest.NewRequest(http.MethodPost, "/api/pgp/identity/import", bytes.NewReader(badBody))
	authRequest(srv, badReq)
	badRec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentityImport)(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong passphrase, got %d", badRec.Code)
	}
}

func TestPGPIdentityImportRevokedKeyReportsRevoked(t *testing.T) {
	srv := newTestServer(t)

	key, err := crypto.PGP().KeyGeneration().AddUserId("Revoked", "revoked@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := key.GetEntity().Revoke(packet.NoReason, "test revocation", &packet.Config{}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	armored, err := key.Armor()
	if err != nil {
		t.Fatalf("Armor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"armoredPrivateKey": armored})
	req := httptest.NewRequest(http.MethodPost, "/api/pgp/identity/import", bytes.NewReader(body))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentityImport)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var importResp pgpIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&importResp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if !importResp.Revoked {
		t.Fatalf("expected revoked=true on import response, got %+v", importResp)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/pgp/identity", nil)
	authRequest(srv, getReq)
	getRec := httptest.NewRecorder()
	srv.withAuth(srv.handlePGPIdentity)(getRec, getReq)
	var getResp pgpIdentityResponse
	if err := json.NewDecoder(getRec.Body).Decode(&getResp); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if !getResp.Revoked {
		t.Fatalf("expected revoked=true on GET response, got %+v", getResp)
	}
}
