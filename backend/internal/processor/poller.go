package processor

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	imapadapter "llama-lab/backend/internal/adapters/imap"
	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/redaction"
	"llama-lab/backend/internal/state"
	"llama-lab/backend/internal/users"

	"github.com/SherClockHolmes/webpush-go"
)

// maxConcurrentUserTicks bounds how many user mailboxes are polled in
// parallel. The shared Llama client serializes classify calls anyway, so
// this mainly overlaps IMAP fetch latency across users.
const maxConcurrentUserTicks = 4

// Poller polls every active user's mailbox each tick. Global config (scan
// interval, rate-limit policy, labels, redaction) is shared; IMAP
// credentials, checkpoint/processed state, tuning prompt, and notification
// preferences are loaded per user.
type Poller struct {
	cfg     config.Config
	cfgMu   sync.RWMutex
	cfgPath string

	log   *logging.Logger
	users *users.Store
	// globalStore holds install-wide state: the sticky AI-credits flag for
	// the one shared LLM backend. Per-user mailbox state lives in stores.
	globalStore   *state.Store
	health        *health.Service
	llama         llama.Client
	redaction     *redaction.Engine
	nativeSenders []NativeSender
	cancel        context.CancelFunc
	tickSem       chan struct{}

	stateDir    string
	configDir   string
	imapKeyPath string

	userMu      sync.Mutex
	stores      map[string]*state.Store
	mailClients map[string]*mailClientEntry
	rate        map[string][]time.Time
}

type mailClientEntry struct {
	client  imapadapter.Client
	modTime time.Time
}

// userCtx bundles one user's per-tick dependencies.
type userCtx struct {
	id       string
	username string
	store    *state.Store
	mail     imapadapter.Client
	tuning   string
	settings config.UserNotificationSettings
}

func New(cfg config.Config, log *logging.Logger, globalStore *state.Store, usersStore *users.Store, stateDir, configDir string, healthSvc *health.Service, llamaClient llama.Client) (*Poller, error) {
	re, err := redaction.New(cfg.Redaction.Patterns)
	if err != nil {
		return nil, err
	}
	p := &Poller{
		cfg:           cfg,
		log:           log,
		users:         usersStore,
		globalStore:   globalStore,
		health:        healthSvc,
		llama:         llamaClient,
		redaction:     re,
		nativeSenders: NewNativeSendersFromEnv(log),
		stateDir:      stateDir,
		configDir:     configDir,
		imapKeyPath:   envOrDefaultProc("IMAP_CONFIG_KEY_FILE", "/llama_lab/private/imap-config.key"),
		stores:        map[string]*state.Store{},
		mailClients:   map[string]*mailClientEntry{},
		rate:          map[string][]time.Time{},
	}
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	return p, nil
}

func envOrDefaultProc(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (p *Poller) userStateDir(userID string) string {
	return filepath.Join(p.stateDir, "users", userID)
}

func (p *Poller) userConfigDir(userID string) string {
	return filepath.Join(p.configDir, "users", userID)
}

func (p *Poller) userIMAPConfigPath(userID string) string {
	return filepath.Join(p.userConfigDir(userID), "imap-config.json")
}

func (p *Poller) userTuningPath(userID string) string {
	return filepath.Join(p.userConfigDir(userID), "tuning.md")
}

func (p *Poller) userSettingsPath(userID string) string {
	return filepath.Join(p.userConfigDir(userID), "config.yaml")
}

func (p *Poller) userStore(userID string) (*state.Store, error) {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if st, ok := p.stores[userID]; ok {
		return st, nil
	}
	st, err := state.New(p.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	p.stores[userID] = st
	return st, nil
}

// userMailClient returns the cached IMAP client for a user, rebuilding it
// when their encrypted credential file changed on disk.
func (p *Poller) userMailClient(userID string, configModTime time.Time) imapadapter.Client {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if entry, ok := p.mailClients[userID]; ok && entry.modTime.Equal(configModTime) {
		return entry.client
	}
	client := imapadapter.NewAPIClientFromStoredConfig(p.userIMAPConfigPath(userID), p.imapKeyPath)
	p.mailClients[userID] = &mailClientEntry{client: client, modTime: configModTime}
	return client
}

func (p *Poller) SetConfigPath(path string) {
	p.cfgMu.Lock()
	p.cfgPath = strings.TrimSpace(path)
	p.cfgMu.Unlock()
}

func (p *Poller) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	interval := time.Duration(p.cfg.Scan.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 90 * time.Second
	}

	p.log.Info("poller started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopped")
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Poller) TriggerNow() {
	p.tick()
}

// TriggerUnreadSweep resets every active user's checkpoint so the next tick
// reconsiders all unread mail, then runs a tick.
func (p *Poller) TriggerUnreadSweep() {
	all, err := p.users.List()
	if err != nil {
		p.log.Error("failed to list users for unread sweep", "error", err.Error())
	} else {
		for _, u := range all {
			if !u.Active {
				continue
			}
			store, err := p.userStore(u.ID)
			if err != nil {
				p.log.Error("failed to open user store for unread sweep", "user_id", u.ID, "error", err.Error())
				continue
			}
			if err := store.SetCheckpoint(""); err != nil {
				p.log.Error("failed to reset checkpoint for unread sweep", "user_id", u.ID, "error", err.Error())
			}
		}
	}
	p.tick()
}

// UpdateConfig swaps the global config and rebuilds the shared redaction
// engine when the patterns changed (previously edits to redaction patterns
// never took effect until restart).
func (p *Poller) UpdateConfig(cfg config.Config) {
	p.cfgMu.Lock()
	patternsChanged := !slices.Equal(p.cfg.Redaction.Patterns, cfg.Redaction.Patterns)
	p.cfg = cfg
	if patternsChanged {
		if re, err := redaction.New(cfg.Redaction.Patterns); err == nil {
			p.redaction = re
		} else {
			p.log.Error("failed to rebuild redaction engine after config update", "error", err.Error())
		}
	}
	p.cfgMu.Unlock()
}

func (p *Poller) currentConfig() config.Config {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

func (p *Poller) currentRedaction() *redaction.Engine {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.redaction
}

func (p *Poller) tick() {
	p.reloadConfigIfNeeded()

	// acquire semaphore; if another tick is running, log that we're waiting
	select {
	case <-p.tickSem:
		// acquired immediately
	default:
		p.log.Info("poll tick waiting for previous tick to finish")
		<-p.tickSem
	}
	defer func() { p.tickSem <- struct{}{} }()

	all, err := p.users.List()
	if err != nil {
		p.log.Error("failed to list users for poll tick", "error", err.Error())
		p.health.MarkUnhealthy("users store unreadable")
		return
	}

	sem := make(chan struct{}, maxConcurrentUserTicks)
	var wg sync.WaitGroup
	var resMu sync.Mutex
	usersPolled := 0
	usersFailed := 0

	for _, u := range all {
		if !u.Active {
			continue
		}
		fi, err := os.Stat(p.userIMAPConfigPath(u.ID))
		if err != nil {
			// No mailbox configured for this user yet — nothing to poll.
			continue
		}
		usersPolled++
		wg.Add(1)
		sem <- struct{}{}
		go func(u users.User, modTime time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("user poll tick panic", "user_id", u.ID, "panic", fmt.Sprint(r))
					resMu.Lock()
					usersFailed++
					resMu.Unlock()
				}
			}()
			if err := p.tickUser(u, modTime); err != nil {
				resMu.Lock()
				usersFailed++
				resMu.Unlock()
			}
		}(u, fi.ModTime())
	}
	wg.Wait()

	p.log.Info("poll tick completed", "users_polled", strconv.Itoa(usersPolled), "users_failed", strconv.Itoa(usersFailed))

	// Fault isolation: one broken mailbox must not restart the container.
	// Only flip global health when every polled mailbox failed.
	if usersPolled > 0 && usersFailed == usersPolled {
		p.health.MarkUnhealthy("imap unreachable for all users")
		return
	}
	p.health.MarkHealthy()
}

// tickUser polls one user's mailbox. Errors are logged with the user id and
// reported to the caller for the all-users-failed health check; they never
// affect other users.
func (p *Poller) tickUser(u users.User, imapConfigModTime time.Time) error {
	store, err := p.userStore(u.ID)
	if err != nil {
		p.log.Error("failed to open user state store", "user_id", u.ID, "error", err.Error())
		return err
	}
	if err := store.Cleanup(30); err != nil {
		p.log.Error("state cleanup failed", "user_id", u.ID, "error", err.Error())
	}

	settings, err := config.LoadUserSettings(p.userSettingsPath(u.ID))
	if err != nil {
		p.log.Error("failed to load user settings, using defaults", "user_id", u.ID, "error", err.Error())
		settings = config.DefaultUserSettings()
	}

	tuning := ""
	if b, err := os.ReadFile(p.userTuningPath(u.ID)); err == nil {
		tuning = strings.TrimSpace(string(b))
	}

	uc := userCtx{
		id:       u.ID,
		username: u.Username,
		store:    store,
		mail:     p.userMailClient(u.ID, imapConfigModTime),
		tuning:   tuning,
		settings: settings.Notifications,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	checkpoint := store.Checkpoint()
	messages, nextCheckpoint, err := uc.mail.ListUnreadInbox(ctx, checkpoint)
	if err != nil {
		p.log.Error("fetch unread inbox failed", "user_id", u.ID, "error", err.Error())
		return err
	}

	processedCount := 0
	skippedSeenCount := 0
	failedCount := 0
	rateLimitedCount := 0
	for _, msg := range messages {
		if store.Seen(msg.ID) {
			skippedSeenCount++
			continue
		}
		if !p.allowByRate(u.ID) {
			p.log.Info("rate limit reached, deferring remaining emails", "user_id", u.ID)
			rateLimitedCount = len(messages) - processedCount - skippedSeenCount - failedCount
			break
		}
		messageCtx, messageCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		err := p.handleMessage(messageCtx, uc, msg)
		messageCancel()
		if err != nil {
			failedCount++
			p.log.Error("message processing failed", "user_id", u.ID, "message_id", msg.ID, "error", err.Error())
			_ = store.AddDecision(state.Decision{
				MessageID: msg.ID,
				Sender:    msg.Sender,
				SentTo:    msg.SentTo,
				Subject:   msg.Subject,
				Status:    "failed",
				Detail:    err.Error(),
			})
			// Retire the message so it is not retried on the next tick.
			_ = store.MarkProcessed(msg.ID)
			p.maybeSendPushNotification(uc, msg, "", nil)
			p.maybeSendNativePushNotification(uc, msg, "", nil)
			continue
		}
		processedCount++
	}

	if nextCheckpoint != "" {
		if err := store.SetCheckpoint(nextCheckpoint); err != nil {
			p.log.Error("failed to persist checkpoint", "user_id", u.ID, "error", err.Error())
		}
	}

	p.log.Info(
		"user poll tick summary",
		"user_id", u.ID,
		"username", u.Username,
		"fetched", strconv.Itoa(len(messages)),
		"processed", strconv.Itoa(processedCount),
		"skipped_seen", strconv.Itoa(skippedSeenCount),
		"failed", strconv.Itoa(failedCount),
		"deferred_rate_limited", strconv.Itoa(rateLimitedCount),
	)
	return nil
}

func (p *Poller) reloadConfigIfNeeded() {
	p.cfgMu.RLock()
	path := p.cfgPath
	p.cfgMu.RUnlock()
	if strings.TrimSpace(path) == "" {
		return
	}
	next, err := config.Load(path)
	if err != nil {
		p.log.Error("failed to reload config for poll tick", "error", err.Error())
		return
	}
	p.UpdateConfig(next)
}

// recentDecisionsContext returns a short summary of the last N applied decisions to give Llama labelling context.
func recentDecisionsContext(store *state.Store, limit int) string {
	all := store.Decisions(50)
	var applied []state.Decision
	for _, d := range all {
		if d.Status == "applied" && d.Label != "" {
			applied = append(applied, d)
			if len(applied) >= limit {
				break
			}
		}
	}
	if len(applied) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Recent labeling decisions for reference:\n")
	for _, d := range applied {
		sb.WriteString("- From: ")
		sb.WriteString(d.Sender)
		if d.Subject != "" {
			sb.WriteString(", Subject: ")
			sb.WriteString(d.Subject)
		}
		sb.WriteString(" → Label: ")
		sb.WriteString(d.Label)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (p *Poller) handleMessage(ctx context.Context, uc userCtx, msg imapadapter.Message) error {
	cfg := p.currentConfig()

	body := strings.TrimSpace(msg.Body)
	if len(body) > 2000 {
		body = body[:2000]
	}
	redacted := p.currentRedaction().Apply(body)

	decisionsCtx := recentDecisionsContext(uc.store, 10)
	bodyWithContext := redacted
	if decisionsCtx != "" {
		if bodyWithContext != "" {
			bodyWithContext = redacted + "\n---\n" + decisionsCtx
		} else {
			bodyWithContext = decisionsCtx
		}
	}

	label, err := classifyWithRetry(ctx, p.llama, cfg.Labels.Allowlist, msg.Sender, msg.Subject, bodyWithContext, uc.tuning)
	if err != nil {
		if isAICreditsExhaustedError(err) {
			p.flagAICreditsExhausted()
		}
		return err
	}
	// A successful classification means Llama has credits again; clear any flag.
	p.clearAICreditsExhausted()
	p.log.Info("classification result", "user_id", uc.id, "message_id", msg.ID, "raw_label", strings.TrimSpace(label), "sender", msg.Sender, "subject", msg.Subject)
	selected := llama.SelectLabelFromText(cfg.Labels.Allowlist, label)
	if selected == "" {
		p.log.Info("classification skipped", "user_id", uc.id, "message_id", msg.ID, "reason", "no known label returned", "raw_label", strings.TrimSpace(label), "allowlist_count", strconv.Itoa(len(cfg.Labels.Allowlist)))
		_ = uc.store.AddDecision(state.Decision{
			MessageID: msg.ID,
			Sender:    msg.Sender,
			SentTo:    msg.SentTo,
			Subject:   msg.Subject,
			Status:    "skipped",
			Detail:    "no known label returned",
		})
		if err := uc.store.MarkProcessed(msg.ID); err != nil {
			return err
		}
		p.maybeSendPushNotification(uc, msg, "", nil)
		p.maybeSendNativePushNotification(uc, msg, "", nil)
		return nil
	}
	keywords := keywordsForSelectedLabel(selected, cfg.Labels.KeywordMappings)
	p.log.Info(
		"applying label",
		"user_id", uc.id,
		"message_id", msg.ID,
		"selected_label", selected,
		"keywords", strings.Join(keywords, ","),
		"sender", msg.Sender,
		"subject", msg.Subject,
	)
	if err := applyKeywordsWithRetry(ctx, uc.mail, msg.ID, keywords); err != nil {
		p.log.Error("label apply failed", "user_id", uc.id, "message_id", msg.ID, "selected_label", selected, "error", err.Error())
		return err
	}
	p.log.Info("label applied", "user_id", uc.id, "message_id", msg.ID, "selected_label", selected, "keywords", strings.Join(keywords, ","))
	if err := uc.store.MarkProcessed(msg.ID); err != nil {
		return err
	}
	if err := uc.store.AddDecision(state.Decision{
		MessageID: msg.ID,
		Sender:    msg.Sender,
		SentTo:    msg.SentTo,
		Subject:   msg.Subject,
		Label:     selected,
		Status:    "applied",
		Detail:    "label applied successfully",
	}); err != nil {
		return err
	}
	p.maybeSendPushNotification(uc, msg, selected, keywords)
	p.maybeSendNativePushNotification(uc, msg, selected, keywords)
	return nil
}

func (p *Poller) maybeSendPushNotification(uc userCtx, msg imapadapter.Message, selectedLabel string, messageKeywords []string) {
	cfg := p.currentConfig()
	if !shouldSendNotification(uc.settings, selectedLabel, messageKeywords) {
		p.log.Info(
			"new-email push notification skipped",
			"reason", "notification mode/keywords did not match",
			"user_id", uc.id,
			"message_id", msg.ID,
			"mode", strings.ToLower(strings.TrimSpace(uc.settings.Mode)),
			"selected_label", strings.TrimSpace(selectedLabel),
			"message_keywords", strings.Join(messageKeywords, ","),
			"configured_keywords", strings.Join(uc.settings.Keywords, ","),
		)
		return
	}

	subs := uc.store.ListNotificationSubscriptions()
	if len(subs) == 0 {
		p.log.Info(
			"new-email push notification skipped",
			"reason", "no active push subscriptions",
			"user_id", uc.id,
			"message_id", msg.ID,
		)
		return
	}

	privateKeyPath := strings.TrimSpace(cfg.Notifications.PrivateKeyPath)
	publicKey := strings.TrimSpace(cfg.Notifications.PublicKey)
	if privateKeyPath == "" || publicKey == "" {
		p.log.Error("notifications enabled but vapid key material missing")
		return
	}

	privateKey, err := loadVAPIDPrivateKey(privateKeyPath)
	if err != nil {
		p.log.Error("failed to load notification private key", "error", err.Error())
		return
	}

	title := "New Email"
	body := buildNotificationBody(msg)
	payloadBytes, err := json.Marshal(map[string]any{
		"title": title,
		"body":  body,
		"url":   "/read",
		"tag":   fmt.Sprintf("llama-mail-email-%s", strings.TrimSpace(msg.ID)),
	})
	if err != nil {
		p.log.Error("failed to marshal notification payload", "error", err.Error())
		return
	}

	options := &webpush.Options{
		Subscriber:      "mailto:noreply@localhost",
		VAPIDPublicKey:  publicKey,
		VAPIDPrivateKey: privateKey,
		TTL:             300,
	}

	sent := 0
	failed := 0
	staleEndpoints := []string{}
	for _, sub := range subs {
		resp, err := webpush.SendNotification(payloadBytes, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				Auth:   sub.Auth,
				P256dh: sub.P256DH,
			},
		}, options)
		if err != nil {
			failed++
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			sent++
			continue
		}
		failed++
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
			staleEndpoints = append(staleEndpoints, sub.Endpoint)
		}
	}

	removed := 0
	for _, endpoint := range staleEndpoints {
		ok, err := uc.store.RemoveNotificationSubscription(endpoint)
		if err == nil && ok {
			removed++
		}
	}

	p.log.Info(
		"new-email push notification attempt",
		"user_id", uc.id,
		"message_id", msg.ID,
		"subscriptions", strconv.Itoa(len(subs)),
		"sent", strconv.Itoa(sent),
		"failed", strconv.Itoa(failed),
		"removed_stale", strconv.Itoa(removed),
	)
}

func (p *Poller) maybeSendNativePushNotification(uc userCtx, msg imapadapter.Message, selectedLabel string, messageKeywords []string) {
	if !shouldSendNotification(uc.settings, selectedLabel, messageKeywords) {
		return
	}

	devices := uc.store.ListNativeDevices()
	if len(devices) == 0 {
		return
	}
	if len(p.nativeSenders) == 0 {
		p.log.Info(
			"new-email native notification skipped",
			"user_id", uc.id,
			"message_id", msg.ID,
			"reason", "no native sender configured",
		)
		return
	}

	title, body := buildNativeNotificationText(msg)
	notification := NativePushMessage{
		Title: title,
		Body:  body,
		// title/body are duplicated into data so a mobile client that
		// renders its own notification from the data payload shows the
		// sender and subject instead of a generic fallback.
		Data: map[string]string{
			"messageId": strings.TrimSpace(msg.ID),
			"sender":    strings.TrimSpace(msg.Sender),
			"subject":   strings.TrimSpace(msg.Subject),
			"title":     title,
			"body":      body,
			"url":       "/read",
		},
	}

	sent := 0
	failed := 0
	removed := 0
	for _, device := range devices {
		sender := SelectNativeSender(p.nativeSenders, device.Platform)
		if sender == nil {
			failed++
			p.log.Error(
				"native notification failed",
				"user_id", uc.id,
				"message_id", msg.ID,
				"device_id", strings.TrimSpace(device.DeviceID),
				"platform", strings.TrimSpace(device.Platform),
				"error", "no sender for platform",
			)
			continue
		}

		sendCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := sender.Send(sendCtx, device, notification)
		cancel()
		if err != nil {
			failed++
			// TODO(server-side management): a failed send (relay unreachable,
			// upstream 5xx, or a 429 when the relay's per-server rate limit is
			// exceeded) currently drops the push — the email still syncs in-app,
			// but no notification fires. Add server-side handling: honor the
			// relay's Retry-After, queue and re-attempt over-limit / transient
			// failures with backoff, and surface persistent failures to the user.
			if errors.Is(err, ErrNativeDeviceStale) && strings.TrimSpace(device.DeviceID) != "" {
				ok, rmErr := uc.store.RemoveNativeDevice(device.DeviceID)
				if rmErr == nil && ok {
					removed++
				}
			}
			p.log.Error(
				"native notification failed",
				"user_id", uc.id,
				"message_id", msg.ID,
				"device_id", strings.TrimSpace(device.DeviceID),
				"platform", strings.TrimSpace(device.Platform),
				"sender", sender.Name(),
				"error", err.Error(),
			)
			continue
		}

		sent++
	}

	p.log.Info(
		"new-email native notification attempt",
		"user_id", uc.id,
		"message_id", msg.ID,
		"devices", strconv.Itoa(len(devices)),
		"sent", strconv.Itoa(sent),
		"failed", strconv.Itoa(failed),
		"removed_stale", strconv.Itoa(removed),
	)
}

func shouldSendNotification(settings config.UserNotificationSettings, selectedLabel string, messageKeywords []string) bool {
	mode := strings.ToLower(strings.TrimSpace(settings.Mode))
	switch mode {
	case "none", "":
		return false
	case "all":
		return true
	case "keywords":
		selected := strings.TrimSpace(selectedLabel)
		if selected != "" {
			messageKeywords = append([]string{selected}, messageKeywords...)
		}

		enabled := map[string]bool{}
		for _, keyword := range settings.Keywords {
			clean := strings.ToLower(strings.TrimSpace(keyword))
			if clean != "" {
				enabled[clean] = true
			}
		}
		if len(enabled) == 0 {
			return false
		}

		for _, keyword := range messageKeywords {
			key := strings.ToLower(strings.TrimSpace(keyword))
			if key != "" && enabled[key] {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func buildNotificationBody(msg imapadapter.Message) string {
	from := strings.TrimSpace(msg.Sender)
	subject := strings.TrimSpace(msg.Subject)
	if from == "" && subject == "" {
		return "You have a new email."
	}
	if from == "" {
		return fmt.Sprintf("Subject: %s", subject)
	}
	if subject == "" {
		return fmt.Sprintf("From: %s", from)
	}
	return fmt.Sprintf("From %s: %s", from, subject)
}

// buildNativeNotificationText renders a mobile push as a mail app would:
// the sender is the notification title and the subject its body, so the
// user sees who it is from and what it is about rather than a generic
// "New Email".
func buildNativeNotificationText(msg imapadapter.Message) (title, body string) {
	from := strings.TrimSpace(msg.Sender)
	subject := strings.TrimSpace(msg.Subject)
	title = from
	if title == "" {
		title = "New Email"
	}
	body = subject
	if body == "" {
		body = "You have a new email."
	}
	return title, body
}

func loadVAPIDPrivateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", errors.New("vapid pem block missing")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	return encodeVAPIDPrivateKey(key), nil
}

func encodeVAPIDPrivateKey(key *ecdsa.PrivateKey) string {
	scalar := key.D.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(scalar):], scalar)
	return base64.RawURLEncoding.EncodeToString(out)
}

func classifyWithRetry(ctx context.Context, c llama.Client, labels []string, sender, subject, body, tuning string) (string, error) {
	var out string
	var err error
	for i := 0; i < 3; i++ {
		out, err = c.Classify(ctx, labels, sender, subject, body, tuning)
		if err == nil && out != "" {
			return out, nil
		}
		if err != nil && isPermanentLlamaClassifyError(err) {
			return "", err
		}
		if err == nil {
			// Classify returned no error but an empty label — treat as retryable.
			err = fmt.Errorf("llama returned empty label")
		}
		if i < 2 {
			time.Sleep(5 * time.Second)
		}
	}
	return "", err
}

func isPermanentLlamaClassifyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "422") {
		return true
	}
	if strings.Contains(msg, "invalid input") || strings.Contains(msg, "unprocessable") {
		return true
	}
	// Out of AI credits will not recover on retry; stop hammering Llama.
	if isAICreditsExhaustedError(err) {
		return true
	}
	return false
}

// isAICreditsExhaustedError reports whether a classify error is Llama signalling
// that the weekly chat limit / AI credits have been exhausted.
func isAICreditsExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "out of ai credits") ||
		strings.Contains(msg, "weekly chat limit")
}

// flagAICreditsExhausted persists the AI-credits flag, mirrors it onto the
// health status, and logs once on the false->true transition.
func (p *Poller) flagAICreditsExhausted() {
	now := time.Now().UTC().Format(time.RFC3339)
	newly, err := p.globalStore.SetAICreditsExhausted(now)
	if err != nil {
		p.log.Error("failed to persist ai credits exhausted flag", "error", err.Error())
	}
	p.health.SetAICreditsExhausted(now)
	if newly {
		p.log.Error("Llama AI credits exhausted; email classification paused until credits reset",
			"detail", "Llama returned the weekly chat limit response")
	}
}

// clearAICreditsExhausted resets the AI-credits flag after a successful classify.
func (p *Poller) clearAICreditsExhausted() {
	if exhausted, _ := p.globalStore.AICreditsExhausted(); !exhausted {
		return
	}
	cleared, err := p.globalStore.ClearAICreditsExhausted()
	if err != nil {
		p.log.Error("failed to clear ai credits exhausted flag", "error", err.Error())
	}
	p.health.ClearAICreditsExhausted()
	if cleared {
		p.log.Info("Llama AI credits restored; email classification resumed")
	}
}

func applyKeywordsWithRetry(ctx context.Context, c imapadapter.Client, messageID string, keywords []string) error {
	for _, keyword := range keywords {
		if err := applySingleKeywordWithRetry(ctx, c, messageID, keyword); err != nil {
			return err
		}
	}
	return nil
}

func applySingleKeywordWithRetry(ctx context.Context, c imapadapter.Client, messageID, keyword string) error {
	var err error
	for i := 0; i < 3; i++ {
		err = c.EnsureLabel(ctx, keyword)
		if err == nil {
			err = c.ApplyLabel(ctx, messageID, keyword)
		}
		if err == nil {
			return nil
		}
		if i < 2 {
			time.Sleep(30 * time.Second)
		}
	}
	return err
}

func keywordsForSelectedLabel(label string, mappings map[string][]string) []string {
	base := strings.TrimSpace(label)
	if base == "" {
		return []string{}
	}

	out := []string{base}
	for mappedLabel, mappedKeywords := range mappings {
		if !strings.EqualFold(strings.TrimSpace(mappedLabel), base) {
			continue
		}
		for _, keyword := range mappedKeywords {
			if cleaned := strings.TrimSpace(keyword); cleaned != "" {
				out = append(out, cleaned)
			}
		}
		break
	}

	seen := map[string]bool{}
	unique := make([]string, 0, len(out))
	for _, keyword := range out {
		key := strings.ToLower(strings.TrimSpace(keyword))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, strings.TrimSpace(keyword))
	}
	return unique
}

// allowByRate applies the global rate-limit policy as an independent budget
// per user, so one busy mailbox cannot starve the others.
func (p *Poller) allowByRate(userID string) bool {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	cfg := p.currentConfig()
	now := time.Now()
	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	window := p.rate[userID]
	trimmed := make([]time.Time, 0, len(window))
	for _, t := range window {
		if t.After(hourCutoff) {
			trimmed = append(trimmed, t)
		}
	}
	minuteCount := 0
	for _, t := range trimmed {
		if t.After(minuteCutoff) {
			minuteCount++
		}
	}
	if minuteCount >= cfg.RateLimits.PerMinute || len(trimmed) >= cfg.RateLimits.PerHour {
		p.rate[userID] = trimmed
		return false
	}
	p.rate[userID] = append(trimmed, now)
	return true
}
