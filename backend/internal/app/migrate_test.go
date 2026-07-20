package app

import (
	"os"
	"path/filepath"
	"testing"

	"kypost-server/backend/internal/config"
	"kypost-server/backend/internal/logging"
	"kypost-server/backend/internal/users"
)

func TestMigrateLegacySingleUserData(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	logDir := t.TempDir()

	logger, err := logging.New(logDir)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	defer logger.Close()

	// Legacy global files.
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(`{"lastCheckpoint":"42","processed":{}}`), 0o600); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "decisions.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write decisions.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "TUNING.md"), []byte("## Allowed Labels\n- Important\n"), 0o600); err != nil {
		t.Fatalf("write TUNING.md: %v", err)
	}
	configFile := filepath.Join(configDir, "config.yaml")
	legacyYAML := "notifications:\n  mode: keywords\n  keywords:\n    - urgent\n"
	if err := os.WriteFile(configFile, []byte(legacyYAML), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	usersStore, err := users.LoadOrMigrate(configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	admin, err := usersStore.FirstAdmin()
	if err != nil {
		t.Fatalf("FirstAdmin: %v", err)
	}

	// app.Run captures the legacy prefs before LoadOrInit rewrites
	// config.yaml with the trimmed schema; mirror that order here.
	legacyPrefs, legacyPrefsOK := config.LoadLegacyNotificationPrefs(configFile)
	if !legacyPrefsOK {
		t.Fatalf("expected legacy prefs to parse")
	}

	if err := migrateLegacySingleUserData(logger, usersStore, configDir, stateDir, legacyPrefs, legacyPrefsOK); err != nil {
		t.Fatalf("migrateLegacySingleUserData: %v", err)
	}

	userStateDir := filepath.Join(stateDir, "users", admin.ID)
	userConfigDir := filepath.Join(configDir, "users", admin.ID)

	if b, err := os.ReadFile(filepath.Join(userStateDir, "state.json")); err != nil || string(b) == "" {
		t.Fatalf("expected migrated state.json, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(userStateDir, "decisions.json")); err != nil {
		t.Fatalf("expected migrated decisions.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userConfigDir, "tuning.md")); err != nil {
		t.Fatalf("expected migrated tuning.md: %v", err)
	}
	settings, err := config.LoadUserSettings(filepath.Join(userConfigDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadUserSettings: %v", err)
	}
	if settings.Notifications.Mode != "keywords" || len(settings.Notifications.Keywords) != 1 || settings.Notifications.Keywords[0] != "urgent" {
		t.Fatalf("unexpected migrated notification prefs: %+v", settings.Notifications)
	}

	// Running the migration again must not clobber the per-user copies.
	if err := os.WriteFile(filepath.Join(userConfigDir, "tuning.md"), []byte("customized"), 0o600); err != nil {
		t.Fatalf("write customized tuning: %v", err)
	}
	if err := migrateLegacySingleUserData(logger, usersStore, configDir, stateDir, legacyPrefs, legacyPrefsOK); err != nil {
		t.Fatalf("second migrateLegacySingleUserData: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(userConfigDir, "tuning.md"))
	if err != nil || string(b) != "customized" {
		t.Fatalf("second migration clobbered user file: content=%q err=%v", string(b), err)
	}
}
