package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"kypost-server/backend/internal/mfa"
	"kypost-server/backend/internal/processor"
	"kypost-server/backend/internal/state"
)

// approverDevices returns the devices eligible to approve a push-2FA challenge
// for a user. Devices explicitly flagged MFAApprover=true are preferred; if the
// user has push 2FA enabled but no device carries the flag (e.g. devices paired
// before the flag existed), every paired device is treated as an approver so a
// legacy pairing keeps working without a migration.
func approverDevices(store *state.Store) []state.NativeDevice {
	all := store.ListNativeDevices()
	approvers := make([]state.NativeDevice, 0, len(all))
	for _, d := range all {
		if d.MFAApprover {
			approvers = append(approvers, d)
		}
	}
	if len(approvers) > 0 {
		return approvers
	}
	return all
}

// handleMFAPushEnabled toggles push 2FA for the calling user. Enabling requires
// TOTP already enabled (so a fallback always exists) and at least one paired
// approver-eligible device. Disabling has no preconditions.
func (s *Server) handleMFAPushEnabled(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
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
	if req.Enabled {
		if !u.TOTPEnabled {
			http.Error(w, "enable an authenticator app (TOTP) before enabling push approval, so you always have a fallback", http.StatusBadRequest)
			return
		}
		store, err := s.userStore(ac.UserID)
		if err != nil {
			http.Error(w, "failed to open user state", http.StatusInternalServerError)
			return
		}
		if len(approverDevices(store)) == 0 {
			http.Error(w, "pair a device on the Notifications page before enabling push approval", http.StatusBadRequest)
			return
		}
	}
	if _, err := s.users.SetPushMFAEnabled(u.ID, req.Enabled); err != nil {
		http.Error(w, "failed to update push 2fa", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pushMfaEnabled": req.Enabled})
}

// handleNativeDeviceMFA flips a specific device's MFAApprover flag. Ownership is
// guaranteed structurally: storeFor resolves the caller's own state store, so a
// user can only toggle their own devices.
func (s *Server) handleNativeDeviceMFA(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	deviceID := strings.TrimSpace(r.PathValue("deviceId"))
	if deviceID == "" {
		http.Error(w, "deviceId is required", http.StatusBadRequest)
		return
	}
	var req struct {
		Approver bool `json:"approver"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	updated, err := store.SetNativeDeviceMFAApprover(deviceID, req.Approver)
	if err != nil {
		http.Error(w, "failed to update device", http.StatusInternalServerError)
		return
	}
	if !updated {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deviceId": deviceID, "approver": req.Approver})
}

// dispatchPushChallenge fans an MFA-challenge push out to every approver-eligible
// device of userID. Best-effort and asynchronous: it runs in its own goroutine so
// relay latency never blocks login, and dispatch failures are logged only (the
// user can still fall back to TOTP). Delivery goes through
// processor.SendNativePushToDevices — the same pull-mode fallback, stale-device
// cleanup, and health recording every other native push in this app gets —
// scoped to the approver-filtered device list rather than every paired device.
// The data payload is the contract a future kypost-android build must recognize.
//
// UnifiedPush devices are excluded from MFA challenges pending end-to-end encryption
// support; MFA metadata is sensitive and should not traverse unencrypted public
// UnifiedPush brokers (e.g., ntfy.sh). Devices remain usable for mail notifications.
func (s *Server) dispatchPushChallenge(userID, challengeID string) {
	store, err := s.userStore(userID)
	if err != nil {
		s.logger.Error("push mfa: open user state failed", "error", err.Error())
		return
	}
	devices := approverDevices(store)
	if len(devices) == 0 {
		return
	}

	// Filter out unifiedpush devices: MFA challenges contain sensitive metadata
	// and should not traverse unencrypted public brokers until encryption is added.
	filteredDevices := make([]state.NativeDevice, 0, len(devices))
	for _, d := range devices {
		if strings.ToLower(strings.TrimSpace(d.Transport)) == "unifiedpush" {
			continue
		}
		filteredDevices = append(filteredDevices, d)
	}
	if len(filteredDevices) == 0 {
		return
	}

	message := processor.NativePushMessage{
		Title: "Approve sign-in",
		Body:  "Tap to approve or deny a sign-in to your account.",
		Data: map[string]string{
			"type":        "mfa_challenge",
			"challengeId": challengeID,
		},
	}
	_, err = processor.SendNativePushToDevices(context.Background(), s.nativePushDispatcher, s.health, store, filteredDevices, message,
		func(device state.NativeDevice, platform string, sendErr error) {
			s.logger.Error("push mfa: dispatch failed", "device_id", strings.TrimSpace(device.DeviceID), "platform", platform, "error", sendErr.Error())
		})
	if err != nil {
		s.logger.Error("push mfa: dispatch failed", "user_id", userID, "error", err.Error())
	}
}

// handlePushPoll reports the live status of a push challenge. In-memory only, so
// the browser can poll it every ~1.5s. Missing/expired challenges report
// "expired" with a 200 so the client reads a uniform {status} shape.
func (s *Server) handlePushPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	status, ok := s.mfaChallenges.PushStatus(strings.TrimSpace(req.ChallengeID))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// handlePushFinish mints the session for an approved push challenge, consuming
// (deleting) the challenge atomically. Not approved => 409; missing/expired =>
// 401. Authenticated solely by possession of the challengeId (no session cookie),
// exactly like the TOTP finish path.
func (s *Server) handlePushFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	userID, err := s.mfaChallenges.ConsumePushApproval(strings.TrimSpace(req.ChallengeID))
	if err != nil {
		if errors.Is(err, mfa.ErrPushNotApproved) {
			http.Error(w, "challenge not approved", http.StatusConflict)
			return
		}
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	u, err := s.users.Get(userID)
	if err != nil || !u.Active {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

// handlePushRespond is the endpoint a paired mobile device calls to approve or
// deny a login challenge. It authenticates with the device's own
// X-Kypost-Device-Id/X-Kypost-Device-Secret credentials (see
// device_auth.go) — no session cookie. It enforces that the responding
// device's owner is exactly the user the challenge was minted for (a device
// can never approve another user's login), and that the device is still
// permitted to approve.
func (s *Server) handlePushRespond(w http.ResponseWriter, r *http.Request) {
	userID, device, ok := s.deviceAuthFromRequest(r)
	if !ok {
		http.Error(w, "invalid device credentials", http.StatusUnauthorized)
		return
	}
	var req struct {
		ChallengeID string `json:"challengeId"`
		Approve     bool   `json:"approve"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	challengeID := strings.TrimSpace(req.ChallengeID)
	if challengeID == "" {
		http.Error(w, "challengeId is required", http.StatusBadRequest)
		return
	}

	// Load the challenge and enforce that the device's owner is the very user
	// the challenge belongs to. This is the core cross-user protection.
	ch, okCh := s.mfaChallenges.Get(challengeID)
	if !okCh {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	if ch.UserID != userID {
		http.Error(w, "challenge does not belong to this device", http.StatusForbidden)
		return
	}

	// The challenge's owning user must have push 2FA explicitly enabled. A
	// challenge is created for TOTP-only users too (since login always offers
	// TOTP), and every native device defaults to MFAApprover=true for ordinary
	// push notifications regardless of this setting — without this check, any
	// paired device could silently approve a login for a user who never opted
	// into push as a second factor.
	owner, err := s.users.Get(userID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	if !owner.PushMFAEnabled {
		http.Error(w, "push approval is not enabled for this account", http.StatusForbidden)
		return
	}

	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	// The device must be permitted to approve. Under the graceful default (no
	// device flagged as approver) any paired device may approve; once any device
	// is explicitly an approver, only approvers may.
	hasApprover := false
	for _, d := range store.ListNativeDevices() {
		if d.MFAApprover {
			hasApprover = true
			break
		}
	}
	if hasApprover && !device.MFAApprover {
		http.Error(w, "device is not permitted to approve sign-in", http.StatusForbidden)
		return
	}

	status, err := s.mfaChallenges.ResolvePush(challengeID, device.DeviceID, req.Approve)
	if err != nil {
		if errors.Is(err, mfa.ErrChallengeAlreadyResolved) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "challenge already resolved", "status": status})
			return
		}
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}
