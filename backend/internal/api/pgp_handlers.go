package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llama-lab/backend/internal/pgpmail"
)

type pgpIdentityResponse struct {
	Fingerprint string `json:"fingerprint"`
	KeyID       string `json:"keyId"`
	PublicKey   string `json:"publicKey"`
	Source      string `json:"source"`
	CreatedAt   string `json:"createdAt"`
	Revoked     bool   `json:"revoked"`
	Expired     bool   `json:"expired"`
}

func (s *Server) handlePGPIdentityGenerate(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "failed to load user", http.StatusInternalServerError)
		return
	}

	id, err := pgpmail.GenerateIdentity(u.Username, u.Username)
	if err != nil {
		http.Error(w, "failed to generate pgp identity", http.StatusInternalServerError)
		return
	}
	s.storePGPIdentity(w, ac.UserID, id, "generated")
}

func (s *Server) handlePGPIdentityImport(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		ArmoredPrivateKey string `json:"armoredPrivateKey"`
		Passphrase        string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ArmoredPrivateKey) == "" {
		http.Error(w, "armoredPrivateKey is required", http.StatusBadRequest)
		return
	}

	id, err := pgpmail.ImportIdentity(req.ArmoredPrivateKey, req.Passphrase)
	if err != nil {
		http.Error(w, "failed to import pgp identity: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.storePGPIdentity(w, ac.UserID, id, "imported")
}

// storePGPIdentity seals id's private key and persists it to the given
// user, replacing any existing PGP identity, then responds with the public
// view. Shared by generate and import.
func (s *Server) storePGPIdentity(w http.ResponseWriter, userID string, id *pgpmail.Identity, source string) {
	sealed, err := id.SealPrivateKey(s.pgpPrivateKeyPath)
	if err != nil {
		http.Error(w, "failed to seal pgp private key", http.StatusInternalServerError)
		return
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.users.SetPGPIdentity(userID, id.Fingerprint, id.KeyID, id.ArmoredPublicKey, sealed, source, createdAt); err != nil {
		http.Error(w, "failed to store pgp identity", http.StatusInternalServerError)
		return
	}
	status := id.Status()
	writeJSON(w, http.StatusOK, pgpIdentityResponse{
		Fingerprint: id.Fingerprint,
		KeyID:       id.KeyID,
		PublicKey:   id.ArmoredPublicKey,
		Source:      source,
		CreatedAt:   createdAt,
		Revoked:     status.Revoked,
		Expired:     status.Expired,
	})
}

func (s *Server) handlePGPIdentity(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		u, err := s.users.Get(ac.UserID)
		if err != nil {
			http.Error(w, "failed to load user", http.StatusInternalServerError)
			return
		}
		if u.PGPFingerprint == "" {
			http.Error(w, "no pgp identity configured", http.StatusNotFound)
			return
		}
		status, _ := pgpmail.CheckKeyStatus(u.PGPPublicKey)
		writeJSON(w, http.StatusOK, pgpIdentityResponse{
			Fingerprint: u.PGPFingerprint,
			KeyID:       u.PGPKeyID,
			PublicKey:   u.PGPPublicKey,
			Source:      u.PGPKeySource,
			CreatedAt:   u.PGPKeyCreatedAt,
			Revoked:     status.Revoked,
			Expired:     status.Expired,
		})
	case http.MethodDelete:
		if _, err := s.users.ClearPGPIdentity(ac.UserID); err != nil {
			http.Error(w, "failed to delete pgp identity", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
