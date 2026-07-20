package api

import (
	"net/http"
	"strings"

	"kypost-server/backend/internal/state"
	"kypost-server/backend/internal/users"
)

// Headers a paired native client presents on every ongoing request (mail
// sync, contacts sync, App Pull, push-MFA-approve, self-deregister) to prove
// it is a specific device that is still in the account's NativeDevices list.
// Each device has its own secret minted at registration time — there is no
// account-wide shared secret and no legacy query-param fallback.
const (
	headerDeviceID     = "X-Kypost-Device-Id"
	headerDeviceSecret = "X-Kypost-Device-Secret"
)

func deviceCredentialsFromRequest(r *http.Request) (deviceID, deviceSecret string) {
	return strings.TrimSpace(r.Header.Get(headerDeviceID)), r.Header.Get(headerDeviceSecret)
}

// deviceAuthFromRequest resolves and authenticates the paired device calling
// r: it extracts deviceId/deviceSecret from headers, finds which user owns
// deviceId, loads that user's live NativeDevice record by ID, and verifies
// deviceSecret against the stored SecretHash. ok=false covers missing
// headers, an unknown device, a wrong secret, and a deviceId that once
// existed but has since been removed (unpaired) — that last case is what
// makes removing a device an immediate, real revocation.
func (s *Server) deviceAuthFromRequest(r *http.Request) (userID string, device state.NativeDevice, ok bool) {
	deviceID, deviceSecret := deviceCredentialsFromRequest(r)
	if deviceID == "" || deviceSecret == "" {
		return "", state.NativeDevice{}, false
	}
	ownerID, okOwner := s.lookupUserByDevice(deviceID)
	if !okOwner {
		return "", state.NativeDevice{}, false
	}
	store, err := s.userStore(ownerID)
	if err != nil {
		return "", state.NativeDevice{}, false
	}
	dev, okDev := store.GetNativeDevice(deviceID)
	if !okDev {
		return "", state.NativeDevice{}, false
	}
	if !users.VerifySecretHash(dev.SecretHash, deviceSecret) {
		return "", state.NativeDevice{}, false
	}
	// Honor account deactivation on the device path the same way currentUser
	// does on the session path: a deactivated (offboarded/compromised) account
	// must lose device access immediately, not keep it until the device secret
	// is separately purged. Without this check, deactivation/password-reset
	// silently fail to revoke a paired device.
	u, err := s.users.Get(ownerID)
	if err != nil || !u.Active {
		return "", state.NativeDevice{}, false
	}
	return ownerID, dev, true
}
