package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/api"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/processor"
	"llama-lab/backend/internal/state"
	"llama-lab/backend/internal/users"
)

// Run dispatches the process mode and blocks until shutdown for long-running modes.
func Run(args []string) error {
	fs := flag.NewFlagSet("llama-lab", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process mode: daemon, server, all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths := config.Paths{
		ConfigFile: filepath.Join(config.EnvOrDefault("CONFIG_DIR", "/llama_lab/config"), "config.yaml"),
		StateDir:   config.EnvOrDefault("STATE_DIR", "/llama_lab/state"),
		LogDir:     config.EnvOrDefault("LOG_DIR", "/llama_lab/logs"),
	}

	// Capture legacy notification prefs before LoadOrInit rewrites
	// config.yaml with the trimmed multi-user schema (which drops the old
	// global mode/keywords fields the migration needs).
	legacyPrefs, legacyPrefsOK := config.LoadLegacyNotificationPrefs(paths.ConfigFile)

	cfg, err := config.LoadOrInit(paths.ConfigFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Timezone == "" {
		cfg.Timezone = "America/New_York"
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", cfg.Timezone, err)
	}

	// Auto-populate label allowlist from TUNING.md when the config has none.
	if len(cfg.Labels.Allowlist) == 0 {
		if labels := llama.ParseAllowedLabels(llama.LoadTuningText()); len(labels) > 0 {
			cfg.Labels.Allowlist = labels
		}
	}

	logger, err := logging.New(paths.LogDir)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Close()

	store, err := state.New(paths.StateDir)
	if err != nil {
		return fmt.Errorf("create state store: %w", err)
	}

	configDir := config.EnvOrDefault("CONFIG_DIR", "/llama_lab/config")
	usersStore, err := users.LoadOrMigrate(configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		return fmt.Errorf("load users store: %w", err)
	}

	if err := migrateLegacySingleUserData(logger, usersStore, configDir, paths.StateDir, legacyPrefs, legacyPrefsOK); err != nil {
		logger.Error("legacy single-user data migration failed", "error", err.Error())
	}

	clearAllMFAIfRequested(logger, usersStore)

	healthSvc := health.NewService()
	healthSvc.MarkHealthy()

	deps := runDeps{
		cfg:        cfg,
		configPath: paths.ConfigFile,
		configDir:  configDir,
		stateDir:   paths.StateDir,
		logger:     logger,
		store:      store,
		users:      usersStore,
		health:     healthSvc,
	}

	switch *mode {
	case "daemon":
		return runDaemon(deps)
	case "server":
		return runServer(deps)
	case "all":
		return runAll(deps)
	default:
		return errors.New("invalid mode; expected daemon, server, or all")
	}
}

type runDeps struct {
	cfg        config.Config
	configPath string
	configDir  string
	stateDir   string
	logger     *logging.Logger
	store      *state.Store
	users      *users.Store
	health     *health.Service
}

func runDaemon(d runDeps) error {
	llamaClient := newLlamaClient(d.cfg)
	poller, err := processor.New(d.cfg, d.logger, d.store, d.users, d.stateDir, d.configDir, d.health, llamaClient)
	if err != nil {
		return err
	}
	poller.SetConfigPath(d.configPath)
	warmupLlamaOnStartup(d.logger, llamaClient, poller)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go poller.Run()
	d.logger.Info("poller goroutine started")
	go monitorHealth(d.logger, d.health)
	<-stop
	poller.Stop()
	return nil
}

func runServer(d runDeps) error {
	srv := api.NewServer(d.cfg, d.logger, d.health, d.users, nil)
	go srv.StartPickupSweeper(context.Background())
	return srv.Run()
}

func runAll(d runDeps) error {
	// Restore the sticky AI-credits flag onto the health status so a restart
	// keeps surfacing it until a successful classify clears it.
	if exhausted, at := d.store.AICreditsExhausted(); exhausted {
		d.health.SetAICreditsExhausted(at)
	}
	llamaClient := newLlamaClient(d.cfg)
	poller, err := processor.New(d.cfg, d.logger, d.store, d.users, d.stateDir, d.configDir, d.health, llamaClient)
	if err != nil {
		return err
	}
	poller.SetConfigPath(d.configPath)
	srv := api.NewServer(d.cfg, d.logger, d.health, d.users, poller.UpdateConfig)
	srv.SetPoller(poller)
	warmupLlamaOnStartup(d.logger, llamaClient, poller)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go poller.Run()
	d.logger.Info("poller goroutine started")
	go srv.StartPickupSweeper(context.Background())
	go monitorHealth(d.logger, d.health)
	go func() {
		if err := srv.Run(); err != nil {
			d.logger.Error("api server stopped", "error", err.Error())
		}
	}()
	<-stop
	poller.Stop()
	return nil
}

// clearAllMFAIfRequested is a break-glass recovery path for self-hosters
// locked out by MFA with no other admin able to reach the Manage Users page.
// Setting MFA_CLEAR_ALL wipes TOTP/recovery codes/push-MFA for every user on
// every boot until the operator unsets it and restarts; it is intentionally
// not self-disabling since the process cannot safely rewrite the host .env.
func clearAllMFAIfRequested(logger *logging.Logger, usersStore *users.Store) {
	if raw := strings.TrimSpace(os.Getenv("MFA_CLEAR_ALL")); raw == "" {
		return
	} else if enabled, err := strconv.ParseBool(raw); err != nil || !enabled {
		return
	}
	all, err := usersStore.List()
	if err != nil {
		logger.Error("MFA_CLEAR_ALL: failed to list users", "error", err.Error())
		return
	}
	cleared := 0
	for _, u := range all {
		if !u.TOTPEnabled && !u.PushMFAEnabled {
			continue
		}
		if _, err := usersStore.DisableTOTP(u.ID); err != nil {
			logger.Error("MFA_CLEAR_ALL: failed to clear user", "user_id", u.ID, "error", err.Error())
			continue
		}
		cleared++
	}
	logger.Error("MFA_CLEAR_ALL is set: cleared two-factor auth for all users", "users_cleared", strconv.Itoa(cleared))
	logger.Error("MFA_CLEAR_ALL: unset this variable and restart once users have re-enrolled, or it will keep clearing MFA on every boot")
}

func envDurationSeconds(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func monitorHealth(logger *logging.Logger, healthSvc *health.Service) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	threshold := envDurationSeconds("UNHEALTHY_RESTART_SECONDS", 300)
	for range ticker.C {
		st := healthSvc.GetStatus()
		if st.Healthy {
			continue
		}
		if st.UnhealthyFor < int64(threshold) {
			continue
		}
		logger.Error("unhealthy threshold exceeded, requesting container restart", "unhealthy_for_seconds", strconv.FormatInt(st.UnhealthyFor, 10))
		_ = syscall.Kill(1, syscall.SIGTERM)
		os.Exit(2)
	}
}

// newLlamaClient builds the one shared LLM client. config.yaml wins when it
// points somewhere real; the OLLAMA_* env vars are the fallback so existing
// env-only deployments keep working. The persisted legacy config default
// ("http://127.0.0.1:3333" with path "/") predates the Ollama runtime and
// is treated as unset.
func newLlamaClient(cfg config.Config) *llama.HTTPClient {
	const legacyDeadDefault = "http://127.0.0.1:3333"

	baseURL := strings.TrimSpace(cfg.Llama.BaseURL)
	fromConfig := baseURL != "" && baseURL != legacyDeadDefault
	if !fromConfig {
		baseURL = strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
		if baseURL == "" {
			baseURL = strings.TrimSpace(os.Getenv("LLAMA_BASE_URL"))
		}
		if baseURL == "" {
			baseURL = "http://127.0.0.1:11434"
		}
	}

	apiKey := strings.TrimSpace(cfg.Llama.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	}

	classifyPath := ""
	if fromConfig {
		classifyPath = strings.TrimSpace(cfg.Llama.ClassifyPath)
	}
	if classifyPath == "" || classifyPath == "/" {
		classifyPath = strings.TrimSpace(os.Getenv("OLLAMA_GENERATE_PATH"))
	}
	if classifyPath == "" {
		classifyPath = "/api/generate"
	}

	// The default tuning text only backstops callers that pass no per-call
	// tuning (e.g. users who have not customized their prompt yet).
	tuning := llama.LoadTuningText()
	return llama.NewHTTPClient(baseURL, apiKey, classifyPath, tuning, 3*time.Minute)
}

func warmupLlamaOnStartup(logger *logging.Logger, client *llama.HTTPClient, poller *processor.Poller) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		logger.Info("llama startup warmup requested")
		if err := client.Warmup(ctx); err != nil {
			logger.Error("llama startup warmup failed", "error", err.Error())
			return
		}
		logger.Info("llama startup warmup completed")
		logger.Info("processing unread unlabeled mail after startup warmup")
		poller.TriggerUnreadSweep()
	}()
}
