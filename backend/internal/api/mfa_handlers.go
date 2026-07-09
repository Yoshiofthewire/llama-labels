package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"llama-lab/backend/internal/mfa"
	"llama-lab/backend/internal/totp"
	"llama-lab/backend/internal/users"
)

// mfaTOTPIssuer is the issuer label shown by authenticator apps.
const mfaTOTPIssuer = "Llama Labels"

// recoveryCodeCount is how many one-time recovery codes are minted at
// enrollment and on regeneration.
const recoveryCodeCount = 10

func (s *Server) handleMFAStatus(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	// Deliberately not named approverDevices: that identifier is a
	// package-level function (push_mfa_handlers.go) computing the fanout
	// set for an active challenge; this local variable lists every paired
	// device (with its raw approver flag) for the management UI, which is a
	// different, broader set on purpose.
	deviceStatuses := []map[string]any{}
	if store, err := s.userStore(ac.UserID); err == nil {
		for _, d := range store.ListNativeDevices() {
			deviceStatuses = append(deviceStatuses, map[string]any{
				"deviceId":   d.DeviceID,
				"deviceName": d.DeviceName,
				"platform":   d.Platform,
				"approver":   d.MFAApprover,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totpEnabled":            u.TOTPEnabled,
		"recoveryCodesRemaining": len(u.RecoveryCodesHash),
		"pushMfaEnabled":         u.PushMFAEnabled,
		"approverDevices":        deviceStatuses,
	})
}

func (s *Server) handleMFASetup(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	if u.TOTPEnabled {
		http.Error(w, "two-factor auth is already enabled; disable it first", http.StatusConflict)
		return
	}

	secret, err := totp.GenerateSecret()
	if err != nil {
		http.Error(w, "failed to generate secret", http.StatusInternalServerError)
		return
	}
	sealed, err := mfa.SealTOTPSecret(secret, s.totpSecretKeyPath)
	if err != nil {
		http.Error(w, "failed to secure secret", http.StatusInternalServerError)
		return
	}
	if _, err := s.users.SetPendingTOTPSecret(u.ID, sealed); err != nil {
		http.Error(w, "failed to stage secret", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":     secret,
		"otpauthUri": totp.ProvisioningURI(mfaTOTPIssuer, u.Username, secret),
	})
}

func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	if u.TOTPEnabled {
		http.Error(w, "two-factor auth is already enabled", http.StatusConflict)
		return
	}
	if u.TOTPSecretEnc == "" {
		http.Error(w, "start setup before confirming", http.StatusBadRequest)
		return
	}
	secret, err := mfa.OpenTOTPSecret(u.TOTPSecretEnc, s.totpSecretKeyPath)
	if err != nil {
		http.Error(w, "failed to load pending secret", http.StatusInternalServerError)
		return
	}
	if _, valid := totp.Validate(secret, strings.TrimSpace(req.Code), time.Now()); !valid {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}

	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		http.Error(w, "failed to generate recovery codes", http.StatusInternalServerError)
		return
	}
	if _, err := s.users.EnableTOTP(u.ID, time.Now().UTC().Format(time.RFC3339), hashes); err != nil {
		http.Error(w, "failed to enable two-factor auth", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recoveryCodes": codes})
}

func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requirePasswordConfirm(w, r)
	if !ok {
		return
	}
	if _, err := s.users.DisableTOTP(u.ID); err != nil {
		http.Error(w, "failed to disable two-factor auth", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMFARecoveryCodesRegenerate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requirePasswordConfirm(w, r)
	if !ok {
		return
	}
	if !u.TOTPEnabled {
		http.Error(w, "two-factor auth is not enabled", http.StatusBadRequest)
		return
	}
	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		http.Error(w, "failed to generate recovery codes", http.StatusInternalServerError)
		return
	}
	if _, err := s.users.ReplaceRecoveryCodes(u.ID, hashes); err != nil {
		http.Error(w, "failed to store recovery codes", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recoveryCodes": codes})
}

// requirePasswordConfirm decodes a {password} body, loads the caller, and
// re-verifies their password. On any failure it writes the response and
// returns ok=false.
func (s *Server) requirePasswordConfirm(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return users.User{}, false
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return users.User{}, false
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return users.User{}, false
	}
	if !users.VerifyPassword(u, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return users.User{}, false
	}
	return u, true
}

// newRecoveryCodes generates fresh plaintext recovery codes plus their scrypt
// hashes for storage. The plaintext is returned to the caller exactly once.
func (s *Server) newRecoveryCodes() (plaintext []string, hashes []string, err error) {
	plaintext, err = mfa.GenerateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		return nil, nil, err
	}
	hashes = make([]string, 0, len(plaintext))
	for _, c := range plaintext {
		h, err := users.HashPassword(c)
		if err != nil {
			return nil, nil, err
		}
		hashes = append(hashes, h)
	}
	return plaintext, hashes, nil
}
