package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"kypost-server/backend/internal/logging"
	"kypost-server/backend/internal/users"
)

// requirePermissionEnforcement skips tests that rely on POSIX permission
// bits actually being enforced: they don't apply on Windows, and they're
// meaningless when running as root, which bypasses file permission checks
// entirely.
func requirePermissionEnforcement(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test relies on POSIX directory permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed when running as root")
	}
}

func newTestUsersStore(t *testing.T) *users.Store {
	t.Helper()
	configDir := t.TempDir()
	store, err := users.LoadOrMigrate(configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	return store
}

func newTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	logger, err := logging.New(t.TempDir())
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return logger
}

// enableTOTPForTest stages and confirms a TOTP enrollment for the given user
// so tests can exercise clearAllMFAIfRequested against a user with MFA on.
func enableTOTPForTest(t *testing.T, store *users.Store, userID string) {
	t.Helper()
	if _, err := store.SetPendingTOTPSecret(userID, "sealed-secret"); err != nil {
		t.Fatalf("SetPendingTOTPSecret: %v", err)
	}
	if _, err := store.EnableTOTP(userID, "2026-01-01T00:00:00Z", []string{"hash1", "hash2"}); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
}

// TestClearAllMFAIfRequested_FirstBootClearsAndWritesMarker proves scenario
// (a): with the env var set and no marker file present, the first boot
// clears MFA for every enrolled user and writes the one-shot marker file.
func TestClearAllMFAIfRequested_FirstBootClearsAndWritesMarker(t *testing.T) {
	t.Setenv("MFA_CLEAR_ALL", "true")

	stateDir := t.TempDir()
	logger := newTestLogger(t)
	store := newTestUsersStore(t)

	admin, err := store.FirstAdmin()
	if err != nil {
		t.Fatalf("FirstAdmin: %v", err)
	}
	enableTOTPForTest(t, store, admin.ID)

	clearAllMFAIfRequested(logger, store, stateDir)

	got, err := store.Get(admin.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled=false after clear, got true")
	}

	markerPath := filepath.Join(stateDir, mfaClearAllMarkerFile)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected marker file %s to exist after successful clear: %v", markerPath, err)
	}
}

// TestClearAllMFAIfRequested_SecondBootDoesNotReclear proves scenario (b):
// with the env var still set but the marker file now present, a user who
// has freshly re-enrolled in MFA is left untouched by a subsequent boot.
func TestClearAllMFAIfRequested_SecondBootDoesNotReclear(t *testing.T) {
	t.Setenv("MFA_CLEAR_ALL", "true")

	stateDir := t.TempDir()
	logger := newTestLogger(t)
	store := newTestUsersStore(t)

	admin, err := store.FirstAdmin()
	if err != nil {
		t.Fatalf("FirstAdmin: %v", err)
	}

	// Simulate the marker already being present from a prior successful boot.
	markerPath := filepath.Join(stateDir, mfaClearAllMarkerFile)
	if err := os.WriteFile(markerPath, []byte("2026-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	// The user has freshly re-enrolled in MFA since that prior clear.
	enableTOTPForTest(t, store, admin.ID)

	clearAllMFAIfRequested(logger, store, stateDir)

	got, err := store.Get(admin.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled to remain true (marker present, clear should be skipped), got false")
	}
}

// TestClearAllMFAIfRequested_PartialFailureDoesNotWriteMarker proves
// scenario (c): if the clear fails partway (a per-user write errors), the
// marker is not written, so the operator's next boot retries the clear
// instead of silently leaving some users un-cleared but marked "done".
//
// The failure is induced the same way internal/logging/rotate_test.go
// induces write failures: stripping write permission from the users store's
// directory so the atomic-write-via-temp-file-then-rename that
// Store.DisableTOTP performs internally fails with EACCES, while the
// preceding read (List) still succeeds since reading only needs directory
// execute/search permission, not write permission.
func TestClearAllMFAIfRequested_PartialFailureDoesNotWriteMarker(t *testing.T) {
	requirePermissionEnforcement(t)
	t.Setenv("MFA_CLEAR_ALL", "true")

	stateDir := t.TempDir()
	logger := newTestLogger(t)

	configDir := t.TempDir()
	store, err := users.LoadOrMigrate(configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	admin, err := store.FirstAdmin()
	if err != nil {
		t.Fatalf("FirstAdmin: %v", err)
	}
	enableTOTPForTest(t, store, admin.ID)

	if err := os.Chmod(configDir, 0o555); err != nil {
		t.Fatalf("chmod configDir: %v", err)
	}
	defer func() {
		_ = os.Chmod(configDir, 0o755) // restore so t.TempDir() cleanup can remove it
	}()

	clearAllMFAIfRequested(logger, store, stateDir)

	markerPath := filepath.Join(stateDir, mfaClearAllMarkerFile)
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("expected marker file NOT to be written after a partial/failed clear")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking marker file: %v", err)
	}
}
