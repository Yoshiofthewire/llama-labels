package api

import (
	"net/http"
	"strings"
	"time"
)

// handlePGPQRToken mints a short-TTL token (session auth) that a scanning
// device can exchange for the caller's public key via handlePGPQRKey — used
// for in-person QR-based contact key exchange.
func (s *Server) handlePGPQRToken(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}

	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusBadRequest)
		return
	}
	token, expiresAt, err := s.createPairingToken(ac.UserID, 2*time.Minute)
	if err != nil {
		http.Error(w, "failed to create qr token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"expiresAt": expiresAt.Format(time.RFC3339),
		"url":       s.pickupBaseURL() + "/api/pgp/qr/key?t=" + token,
	})
}

// handlePGPQRKey is public and token-gated (no session): it returns the
// token owner's armored public key + display name, for a scanning device to
// offer as a new/updated contact PGP key.
func (s *Server) handlePGPQRKey(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}

	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	userID, err := s.parsePairingTokenUserID(token, time.Now())
	if err != nil {
		http.Error(w, "invalid or expired token", http.StatusForbidden)
		return
	}
	u, err := s.users.Get(userID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        u.Username,
		"fingerprint": u.PGPFingerprint,
		"publicKey":   u.PGPPublicKey,
	})
}
