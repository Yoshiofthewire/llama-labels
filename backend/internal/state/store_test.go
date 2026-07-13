package state

import (
	"testing"
	"time"
)

func TestNotificationSubscriptionsSyncAcrossStoreInstances(t *testing.T) {
	dir := t.TempDir()

	daemonStore, err := New(dir)
	if err != nil {
		t.Fatalf("New daemon store: %v", err)
	}
	serverStore, err := New(dir)
	if err != nil {
		t.Fatalf("New server store: %v", err)
	}

	sub := NotificationSubscription{
		Endpoint:  "https://push.example/endpoint-1",
		Auth:      "auth-token",
		P256DH:    "p256-token",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := serverStore.UpsertNotificationSubscription(sub); err != nil {
		t.Fatalf("UpsertNotificationSubscription: %v", err)
	}

	subs := daemonStore.ListNotificationSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("ListNotificationSubscriptions len = %d, want 1", len(subs))
	}
	if subs[0].Endpoint != sub.Endpoint {
		t.Fatalf("endpoint = %q, want %q", subs[0].Endpoint, sub.Endpoint)
	}
}

func TestUpsertNotificationSubscriptionPreservesLatestSharedState(t *testing.T) {
	dir := t.TempDir()

	daemonStore, err := New(dir)
	if err != nil {
		t.Fatalf("New daemon store: %v", err)
	}
	serverStore, err := New(dir)
	if err != nil {
		t.Fatalf("New server store: %v", err)
	}

	if err := daemonStore.SetCheckpoint("uid-42"); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	sub := NotificationSubscription{
		Endpoint:  "https://push.example/endpoint-2",
		Auth:      "auth-token",
		P256DH:    "p256-token",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := serverStore.UpsertNotificationSubscription(sub); err != nil {
		t.Fatalf("UpsertNotificationSubscription: %v", err)
	}

	reloadedStore, err := New(dir)
	if err != nil {
		t.Fatalf("New reloaded store: %v", err)
	}
	if got := reloadedStore.Checkpoint(); got != "uid-42" {
		t.Fatalf("checkpoint = %q, want %q", got, "uid-42")
	}
}

func TestMarkProcessedDoesNotWipeNotificationSubscriptions(t *testing.T) {
	dir := t.TempDir()

	daemonStore, err := New(dir)
	if err != nil {
		t.Fatalf("New daemon store: %v", err)
	}
	serverStore, err := New(dir)
	if err != nil {
		t.Fatalf("New server store: %v", err)
	}

	sub := NotificationSubscription{
		Endpoint:  "https://push.example/endpoint-3",
		Auth:      "auth-token",
		P256DH:    "p256-token",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := serverStore.UpsertNotificationSubscription(sub); err != nil {
		t.Fatalf("UpsertNotificationSubscription: %v", err)
	}

	// Simulate daemon writing unrelated state after registration.
	if err := daemonStore.MarkProcessed("msg-123"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	reloadedStore, err := New(dir)
	if err != nil {
		t.Fatalf("New reloaded store: %v", err)
	}
	subs := reloadedStore.ListNotificationSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("ListNotificationSubscriptions len = %d, want 1", len(subs))
	}
	if subs[0].Endpoint != sub.Endpoint {
		t.Fatalf("endpoint = %q, want %q", subs[0].Endpoint, sub.Endpoint)
	}
}

func TestNativeDevicesSyncAcrossStoreInstances(t *testing.T) {
	dir := t.TempDir()

	daemonStore, err := New(dir)
	if err != nil {
		t.Fatalf("New daemon store: %v", err)
	}
	serverStore, err := New(dir)
	if err != nil {
		t.Fatalf("New server store: %v", err)
	}

	device := NativeDevice{
		DeviceID:     "device-1",
		Platform:     "android",
		PushToken:    "token-1",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := serverStore.UpsertNativeDevice(device); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}

	devices := daemonStore.ListNativeDevices()
	if len(devices) != 1 {
		t.Fatalf("ListNativeDevices len = %d, want 1", len(devices))
	}
	if devices[0].DeviceID != device.DeviceID {
		t.Fatalf("deviceId = %q, want %q", devices[0].DeviceID, device.DeviceID)
	}
}

func TestUpsertNativeDeviceMergesBySamePushToken(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First registration without a device ID mints one.
	if err := s.UpsertNativeDevice(NativeDevice{Platform: "macos", PushToken: "tok-mac", DeviceName: "Mac 1"}); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}
	first := s.ListNativeDevices()
	if len(first) != 1 || first[0].DeviceID == "" {
		t.Fatalf("ListNativeDevices after first upsert = %+v, want 1 device with minted ID", first)
	}

	// Re-registering the same token+platform without an ID (a re-pair from a
	// fresh deep link) must update the row, not pair the device twice.
	if err := s.UpsertNativeDevice(NativeDevice{Platform: "macos", PushToken: "tok-mac", DeviceName: "Mac 2"}); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}
	merged := s.ListNativeDevices()
	if len(merged) != 1 {
		t.Fatalf("ListNativeDevices len = %d, want 1 (same token must merge)", len(merged))
	}
	if merged[0].DeviceID != first[0].DeviceID {
		t.Fatalf("deviceId = %q, want original %q", merged[0].DeviceID, first[0].DeviceID)
	}
	if merged[0].DeviceName != "Mac 2" {
		t.Fatalf("deviceName = %q, want updated %q", merged[0].DeviceName, "Mac 2")
	}
	if merged[0].RegisteredAt != first[0].RegisteredAt {
		t.Fatalf("registeredAt = %q, want preserved %q", merged[0].RegisteredAt, first[0].RegisteredAt)
	}

	// Same token on a different platform stays a separate device (simulator
	// placeholder tokens collide across platforms).
	if err := s.UpsertNativeDevice(NativeDevice{Platform: "ios", PushToken: "tok-mac"}); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}
	if got := len(s.ListNativeDevices()); got != 2 {
		t.Fatalf("ListNativeDevices len = %d, want 2 (different platform must not merge)", got)
	}
}

func TestSetCheckpointDoesNotWipeNativeDevices(t *testing.T) {
	dir := t.TempDir()

	daemonStore, err := New(dir)
	if err != nil {
		t.Fatalf("New daemon store: %v", err)
	}
	serverStore, err := New(dir)
	if err != nil {
		t.Fatalf("New server store: %v", err)
	}

	device := NativeDevice{
		DeviceID:     "device-2",
		Platform:     "android",
		PushToken:    "token-2",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := serverStore.UpsertNativeDevice(device); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}

	if err := daemonStore.SetCheckpoint("uid-77"); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	reloadedStore, err := New(dir)
	if err != nil {
		t.Fatalf("New reloaded store: %v", err)
	}
	if got := reloadedStore.Checkpoint(); got != "uid-77" {
		t.Fatalf("checkpoint = %q, want %q", got, "uid-77")
	}
	devices := reloadedStore.ListNativeDevices()
	if len(devices) != 1 {
		t.Fatalf("ListNativeDevices len = %d, want 1", len(devices))
	}
	if devices[0].DeviceID != device.DeviceID {
		t.Fatalf("deviceId = %q, want %q", devices[0].DeviceID, device.DeviceID)
	}
}

func TestNativeDeviceMFAApproverToggle(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.UpsertNativeDevice(NativeDevice{DeviceID: "dev-1", Platform: "android", PushToken: "tok-1", UserID: "user-1", MFAApprover: true}); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}

	got, ok := s.GetNativeDevice("dev-1")
	if !ok || !got.MFAApprover || got.UserID != "user-1" {
		t.Fatalf("GetNativeDevice = %+v ok=%v", got, ok)
	}

	updated, err := s.SetNativeDeviceMFAApprover("dev-1", false)
	if err != nil || !updated {
		t.Fatalf("SetNativeDeviceMFAApprover: updated=%v err=%v", updated, err)
	}
	got, _ = s.GetNativeDevice("dev-1")
	if got.MFAApprover {
		t.Fatalf("expected approver cleared, got %+v", got)
	}

	missing, err := s.SetNativeDeviceMFAApprover("nope", true)
	if err != nil || missing {
		t.Fatalf("expected updated=false for unknown device, got updated=%v err=%v", missing, err)
	}
}
