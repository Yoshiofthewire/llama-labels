package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llama-lab/backend/internal/state"
	"llama-lab/backend/internal/users"
)

// pairApproverDevice registers a native device for userID directly in that
// user's state store and warms the subscriber index, returning the credential
// pair a simulated device presents to handlePushRespond. It mirrors the trust
// boundary the real register/pull endpoints use (subscriberId + subscriberHash).
func pairApproverDevice(t *testing.T, srv *Server, userID, deviceID string) (subscriberID, subscriberHash string) {
	t.Helper()
	store, err := srv.userStore(userID)
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	subscriberID, err = store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatalf("GetOrCreateSubscriberID: %v", err)
	}
	srv.userMu.Lock()
	srv.subIndex[subscriberID] = userID
	srv.userMu.Unlock()
	if err := store.UpsertNativeDevice(state.NativeDevice{
		DeviceID:    deviceID,
		Platform:    "android",
		PushToken:   "tok-" + deviceID,
		UserID:      userID,
		MFAApprover: true,
	}); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}
	return subscriberID, srv.pairingSubscriberHash(subscriberID)
}

func TestPushEnableRequiresTOTP(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("nina", "pw-nina", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No TOTP enrolled: enabling push must be rejected.
	rec := doJSONAuth(srv, srv.withAuth(srv.handleMFAPushEnabled), http.MethodPut,
		"/api/mfa/push/enabled", map[string]bool{"enabled": true}, u.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without TOTP: status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPushEnableRequiresDevice(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("omar", "pw-omar", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	enrollTOTP(t, srv, u.ID)
	// TOTP enrolled but no paired device: still rejected.
	rec := doJSONAuth(srv, srv.withAuth(srv.handleMFAPushEnabled), http.MethodPut,
		"/api/mfa/push/enabled", map[string]bool{"enabled": true}, u.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without device: status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPushEnableAndStatusAndDeviceToggle(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("pia", "pw-pia", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	enrollTOTP(t, srv, u.ID)
	pairApproverDevice(t, srv, u.ID, "dev-pia")

	enableRec := doJSONAuth(srv, srv.withAuth(srv.handleMFAPushEnabled), http.MethodPut,
		"/api/mfa/push/enabled", map[string]bool{"enabled": true}, u.ID)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable: status=%d body=%s", enableRec.Code, enableRec.Body.String())
	}

	statusRec := doJSONAuth(srv, srv.withAuth(srv.handleMFAStatus), http.MethodGet, "/api/mfa/status", nil, u.ID)
	var status struct {
		TOTPEnabled     bool `json:"totpEnabled"`
		PushMFAEnabled  bool `json:"pushMfaEnabled"`
		ApproverDevices []struct {
			DeviceID string `json:"deviceId"`
			Approver bool   `json:"approver"`
		} `json:"approverDevices"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !status.PushMFAEnabled || len(status.ApproverDevices) != 1 || !status.ApproverDevices[0].Approver {
		t.Fatalf("status = %+v", status)
	}

	// Toggle the device's approver flag off via the path-scoped endpoint.
	toggleReq := httptest.NewRequest(http.MethodPut, "/api/notifications/native/devices/dev-pia/mfa",
		bytes.NewReader([]byte(`{"approver":false}`)))
	toggleReq.SetPathValue("deviceId", "dev-pia")
	authRequestAs(srv, toggleReq, u.ID)
	toggleRec := httptest.NewRecorder()
	srv.withAuth(srv.handleNativeDeviceMFA)(toggleRec, toggleReq)
	if toggleRec.Code != http.StatusOK {
		t.Fatalf("toggle: status=%d body=%s", toggleRec.Code, toggleRec.Body.String())
	}
	store, _ := srv.userStore(u.ID)
	if d, _ := store.GetNativeDevice("dev-pia"); d.MFAApprover {
		t.Fatalf("expected approver cleared after toggle")
	}
}
