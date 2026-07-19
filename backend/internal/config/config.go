package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kypost-server/backend/internal/fsutil"

	"gopkg.in/yaml.v3"
)

type Paths struct {
	ConfigFile string
	StateDir   string
	LogDir     string
}

// EnvOrDefault returns the trimmed value of the environment variable key, or
// fallback if it is unset or blank after trimming.
func EnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// EnvInt returns the environment variable key parsed as a positive int, or
// fallback if it is unset, unparseable, or not positive.
func EnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

type Config struct {
	Timezone string `yaml:"timezone" json:"timezone"`
	LogLevel string `yaml:"logLevel" json:"logLevel"`

	Classifier struct {
		BaseURL      string `yaml:"baseUrl" json:"baseUrl"`
		APIKey       string `yaml:"apiKey" json:"apiKey"`
		ClassifyPath string `yaml:"classifyPath" json:"classifyPath"`
	} `yaml:"classifier" json:"classifier"`

	Scan struct {
		IntervalSeconds int `yaml:"intervalSeconds" json:"intervalSeconds"`
	} `yaml:"scan" json:"scan"`

	RateLimits struct {
		PerMinute int `yaml:"perMinute" json:"perMinute"`
		PerHour   int `yaml:"perHour" json:"perHour"`
	} `yaml:"rateLimits" json:"rateLimits"`

	Redaction struct {
		Patterns []Pattern `yaml:"patterns" json:"patterns"`
	} `yaml:"redaction" json:"redaction"`

	Labels struct {
		Allowlist       []string            `yaml:"allowlist" json:"allowlist"`
		KeywordMappings map[string][]string `yaml:"keywordMappings" json:"keywordMappings"`
	} `yaml:"labels" json:"labels"`

	Notifications NotificationKeys `yaml:"notifications" json:"-"`
}

// NotificationKeys is the shared VAPID signing identity for the whole
// install. Delivery preferences (mode/keywords) are per-user and live in
// UserSettings instead.
type NotificationKeys struct {
	PublicKey      string `yaml:"publicKey" json:"-"`
	PrivateKeyPath string `yaml:"privateKeyPath" json:"-"`
}

// UserSettings is the small per-user preferences document stored at
// CONFIG_DIR/users/<userID>/config.yaml.
type UserSettings struct {
	Notifications UserNotificationSettings `yaml:"notifications" json:"notifications"`
	Labels        UserLabelSettings        `yaml:"labels" json:"labels"`
}

type UserNotificationSettings struct {
	Mode     string   `yaml:"mode" json:"mode"`
	Keywords []string `yaml:"keywords" json:"keywords"`
}

// UserLabelSettings controls whether the AI classification pipeline
// automatically applies keyword labels for this user. When
// AutoApplyEnabled is false, classification is skipped entirely and every
// message is tagged with the account's default label instead (see
// disabledLabelingFallback in processor/poller.go).
type UserLabelSettings struct {
	AutoApplyEnabled bool `yaml:"autoApplyEnabled" json:"autoApplyEnabled"`
}

func DefaultUserSettings() UserSettings {
	var s UserSettings
	s.Notifications.Mode = "none"
	s.Notifications.Keywords = []string{}
	s.Labels.AutoApplyEnabled = true
	return s
}

// LoadUserSettings reads a per-user settings file, returning defaults if it
// does not exist yet.
func LoadUserSettings(path string) (UserSettings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultUserSettings(), nil
		}
		return UserSettings{}, err
	}
	s := DefaultUserSettings()
	if err := yaml.Unmarshal(b, &s); err != nil {
		return UserSettings{}, err
	}
	if s.Notifications.Keywords == nil {
		s.Notifications.Keywords = []string{}
	}
	return s, nil
}

func SaveUserSettings(path string, s UserSettings) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, b, 0o600)
}

// LoadLegacyNotificationPrefs extracts the pre-multi-user mode/keywords
// fields from a legacy global config.yaml, for one-time migration into the
// first admin user's settings file.
func LoadLegacyNotificationPrefs(path string) (UserNotificationSettings, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return UserNotificationSettings{}, false
	}
	var legacy struct {
		Notifications UserNotificationSettings `yaml:"notifications"`
	}
	if err := yaml.Unmarshal(b, &legacy); err != nil {
		return UserNotificationSettings{}, false
	}
	if strings.TrimSpace(legacy.Notifications.Mode) == "" {
		return UserNotificationSettings{}, false
	}
	return legacy.Notifications, true
}

type Pattern struct {
	Name        string `yaml:"name" json:"name"`
	Regex       string `yaml:"regex" json:"regex"`
	Replacement string `yaml:"replacement" json:"replacement"`
}

func Default() Config {
	cfg := Config{
		Timezone: "America/New_York",
		LogLevel: "info",
	}
	// Empty means "fall back to the OLLAMA_* env vars"; see newClassifierClient.
	cfg.Classifier.BaseURL = ""
	cfg.Classifier.APIKey = ""
	cfg.Classifier.ClassifyPath = ""
	cfg.Scan.IntervalSeconds = 90
	cfg.RateLimits.PerMinute = 10
	cfg.RateLimits.PerHour = 20
	cfg.Redaction.Patterns = []Pattern{
		{Name: "email", Regex: `(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`, Replacement: "[REDACTED_EMAIL]"},
		{Name: "phone", Regex: `\b(?:\+?\d{1,3}[\s.-]?)?(?:\(\d{3}\)|\d{3})[\s.-]?\d{3}[\s.-]?\d{4}\b`, Replacement: "[REDACTED_PHONE]"},
		{Name: "ssn", Regex: `\b\d{3}-\d{2}-\d{4}\b`, Replacement: "[REDACTED_SSN]"},
		{Name: "iban", Regex: `\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b`, Replacement: "[REDACTED_IBAN]"},
		{Name: "card", Regex: `\b(?:\d[ -]*?){13,19}\b`, Replacement: "[REDACTED_CARD]"},
	}
	cfg.Labels.KeywordMappings = map[string][]string{}
	return cfg
}

func LoadOrInit(path string) (Config, error) {
	configDir := filepath.Dir(path)
	if _, err := os.Stat(path); err == nil {
		cfg, err := Load(path)
		if err != nil {
			return Config{}, err
		}
		changed, err := ensureNotificationKeyMaterial(configDir, &cfg)
		if err != nil {
			return Config{}, err
		}
		if changed {
			if err := Save(path, cfg); err != nil {
				return Config{}, err
			}
		}
		return cfg, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Config{}, fmt.Errorf("mkdir config dir: %w", err)
	}
	cfg := Default()
	_, err := ensureNotificationKeyMaterial(configDir, &cfg)
	if err != nil {
		return Config{}, err
	}
	if err := Save(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func ensureNotificationKeyMaterial(configDir string, cfg *Config) (bool, error) {
	changed := false
	if strings.TrimSpace(cfg.Notifications.PrivateKeyPath) == "" {
		cfg.Notifications.PrivateKeyPath = filepath.Join(configDir, "notifications-vapid-private.pem")
		changed = true
	}
	key, err := loadOrCreateNotificationPrivateKey(cfg.Notifications.PrivateKeyPath)
	if err != nil {
		return changed, err
	}
	publicKey := base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.PublicKey.X, key.PublicKey.Y))
	if cfg.Notifications.PublicKey != publicKey {
		cfg.Notifications.PublicKey = publicKey
		changed = true
	}
	return changed, nil
}

// LoadVAPIDPrivateKey reads the notification VAPID private key PEM at path and
// returns it in the base64url raw-scalar form the webpush library expects.
func LoadVAPIDPrivateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", fmt.Errorf("vapid pem block missing")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	scalar := key.D.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(scalar):], scalar)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func loadOrCreateNotificationPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("decode notification private key: pem block missing")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse notification private key: %w", err)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir notification key dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate notification key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal notification key: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create notification key: %w", err)
	}
	if err := pem.Encode(file, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}); err != nil {
		file.Close()
		return nil, fmt.Errorf("write notification key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close notification key: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod notification key: %w", err)
	}
	return key, nil
}
