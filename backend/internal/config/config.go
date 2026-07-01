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
	"strings"

	"gopkg.in/yaml.v3"
)

type Paths struct {
	ConfigFile string
	StateDir   string
	LogDir     string
}

type Config struct {
	Timezone string `yaml:"timezone" json:"timezone"`
	LogLevel string `yaml:"logLevel" json:"logLevel"`

	Llama struct {
		BaseURL      string `yaml:"baseUrl" json:"baseUrl"`
		APIKey       string `yaml:"apiKey" json:"apiKey"`
		ClassifyPath string `yaml:"classifyPath" json:"classifyPath"`
	} `yaml:"llama" json:"llama"`

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

	Notifications NotificationSettings `yaml:"notifications" json:"notifications"`
}

type NotificationSettings struct {
	Mode           string `yaml:"mode" json:"mode"`
	Folder         string `yaml:"folder" json:"folder"`
	PublicKey      string `yaml:"publicKey" json:"publicKey"`
	PrivateKeyPath string `yaml:"privateKeyPath" json:"privateKeyPath"`
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
	cfg.Llama.BaseURL = "http://127.0.0.1:3333"
	cfg.Llama.APIKey = ""
	cfg.Llama.ClassifyPath = "/"
	cfg.Scan.IntervalSeconds = 90
	cfg.RateLimits.PerMinute = 10
	cfg.RateLimits.PerHour = 20
	cfg.Redaction.Patterns = []Pattern{
		{Name: "email", Regex: `(?i)\\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\\.[A-Z]{2,}\\b`, Replacement: "[REDACTED_EMAIL]"},
		{Name: "phone", Regex: `\\b(?:\\+?\\d{1,3}[\\s.-]?)?(?:\\(\\d{3}\\)|\\d{3})[\\s.-]?\\d{3}[\\s.-]?\\d{4}\\b`, Replacement: "[REDACTED_PHONE]"},
		{Name: "ssn", Regex: `\\b\\d{3}-\\d{2}-\\d{4}\\b`, Replacement: "[REDACTED_SSN]"},
		{Name: "iban", Regex: `\\b[A-Z]{2}\\d{2}[A-Z0-9]{10,30}\\b`, Replacement: "[REDACTED_IBAN]"},
		{Name: "card", Regex: `\\b(?:\\d[ -]*?){13,19}\\b`, Replacement: "[REDACTED_CARD]"},
	}
	cfg.Labels.KeywordMappings = map[string][]string{}
	cfg.Notifications.Mode = "none"
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
