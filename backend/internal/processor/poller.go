package processor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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

	"github.com/SherClockHolmes/webpush-go"
)

type Poller struct {
	cfg       config.Config
	cfgMu     sync.RWMutex
	cfgPath   string
	log       *logging.Logger
	store     *state.Store
	health    *health.Service
	mail      imapadapter.Client
	llama     llama.Client
	redaction *redaction.Engine
	cancel    context.CancelFunc
	mu        sync.Mutex
	tickSem   chan struct{}
	processed []time.Time
	novu      NovuConfig
}

type NovuConfig struct {
	SecretKey  string
	WorkflowID string
	APIBase    string
}

func New(cfg config.Config, log *logging.Logger, store *state.Store, healthSvc *health.Service, mailClient imapadapter.Client, llamaClient llama.Client, novuCfg NovuConfig) (*Poller, error) {
	re, err := redaction.New(cfg.Redaction.Patterns)
	if err != nil {
		return nil, err
	}
	novuCfg.SecretKey = strings.TrimSpace(novuCfg.SecretKey)
	novuCfg.WorkflowID = strings.TrimSpace(novuCfg.WorkflowID)
	novuCfg.APIBase = strings.TrimRight(strings.TrimSpace(novuCfg.APIBase), "/")
	if novuCfg.APIBase == "" {
		novuCfg.APIBase = "https://api.novu.co"
	}
	p := &Poller{cfg: cfg, log: log, store: store, health: healthSvc, mail: mailClient, llama: llamaClient, redaction: re, processed: []time.Time{}, novu: novuCfg}
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	return p, nil
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

func (p *Poller) TriggerUnreadSweep() {
	if err := p.store.SetCheckpoint(""); err != nil {
		p.log.Error("failed to reset checkpoint for unread sweep", "error", err.Error())
	}
	p.tick()
}

func (p *Poller) UpdateConfig(cfg config.Config) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
}

func (p *Poller) currentConfig() config.Config {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
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

	if err := p.store.Cleanup(30); err != nil {
		p.log.Error("state cleanup failed", "error", err.Error())
		p.health.MarkUnhealthy("state cleanup failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	checkpoint := p.store.Checkpoint()
	messages, nextCheckpoint, err := p.mail.ListUnreadInbox(ctx, checkpoint)
	if err != nil {
		p.log.Error("fetch unread inbox failed", "error", err.Error())
		p.health.MarkUnhealthy("imap unreachable")
		return
	}

	processedCount := 0
	skippedSeenCount := 0
	failedCount := 0
	rateLimitedCount := 0
	for _, msg := range messages {
		if p.store.Seen(msg.ID) {
			skippedSeenCount++
			continue
		}
		if !p.allowByRate() {
			p.log.Info("rate limit reached, deferring remaining emails")
			rateLimitedCount = len(messages) - processedCount - skippedSeenCount - failedCount
			break
		}
		messageCtx, messageCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		err := p.handleMessage(messageCtx, msg)
		messageCancel()
		if err != nil {
			failedCount++
			p.log.Error("message processing failed", "message_id", msg.ID, "error", err.Error())
			_ = p.store.AddDecision(state.Decision{
				MessageID: msg.ID,
				Sender:    msg.Sender,
				SentTo:    msg.SentTo,
				Subject:   msg.Subject,
				Status:    "failed",
				Detail:    err.Error(),
			})
			// Retire the message so it is not retried on the next tick.
			_ = p.store.MarkProcessed(msg.ID)
			p.maybeSendPushNotification(msg, "", nil)
			p.maybeTriggerNovu(msg, "", nil)
			continue
		}
		processedCount++
	}

	if nextCheckpoint != "" {
		if err := p.store.SetCheckpoint(nextCheckpoint); err != nil {
			p.log.Error("failed to persist checkpoint", "error", err.Error())
		}
	}

	p.log.Info(
		"poll tick summary",
		"fetched", strconv.Itoa(len(messages)),
		"processed", strconv.Itoa(processedCount),
		"skipped_seen", strconv.Itoa(skippedSeenCount),
		"failed", strconv.Itoa(failedCount),
		"deferred_rate_limited", strconv.Itoa(rateLimitedCount),
	)
	p.log.Info("poll tick completed")
	p.health.MarkHealthy()
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
func (p *Poller) recentDecisionsContext(limit int) string {
	all := p.store.Decisions(50)
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

func (p *Poller) handleMessage(ctx context.Context, msg imapadapter.Message) error {
	cfg := p.currentConfig()

	body := strings.TrimSpace(msg.Body)
	if len(body) > 2000 {
		body = body[:2000]
	}
	redacted := p.redaction.Apply(body)

	decisionsCtx := p.recentDecisionsContext(10)
	bodyWithContext := redacted
	if decisionsCtx != "" {
		if bodyWithContext != "" {
			bodyWithContext = redacted + "\n---\n" + decisionsCtx
		} else {
			bodyWithContext = decisionsCtx
		}
	}

	label, err := classifyWithRetry(ctx, p.llama, cfg.Labels.Allowlist, msg.Sender, msg.Subject, bodyWithContext)
	if err != nil {
		if isAICreditsExhaustedError(err) {
			p.flagAICreditsExhausted()
		}
		return err
	}
	// A successful classification means Llama has credits again; clear any flag.
	p.clearAICreditsExhausted()
	p.log.Info("classification result", "message_id", msg.ID, "raw_label", strings.TrimSpace(label), "sender", msg.Sender, "subject", msg.Subject)
	selected := llama.SelectLabelFromText(cfg.Labels.Allowlist, label)
	if selected == "" {
		p.log.Info("classification skipped", "message_id", msg.ID, "reason", "no known label returned", "raw_label", strings.TrimSpace(label), "allowlist_count", strconv.Itoa(len(cfg.Labels.Allowlist)))
		_ = p.store.AddDecision(state.Decision{
			MessageID: msg.ID,
			Sender:    msg.Sender,
			SentTo:    msg.SentTo,
			Subject:   msg.Subject,
			Status:    "skipped",
			Detail:    "no known label returned",
		})
		if err := p.store.MarkProcessed(msg.ID); err != nil {
			return err
		}
		p.maybeSendPushNotification(msg, "", nil)
		p.maybeTriggerNovu(msg, "", nil)
		return nil
	}
	keywords := keywordsForSelectedLabel(selected, cfg.Labels.KeywordMappings)
	p.log.Info(
		"applying label",
		"message_id", msg.ID,
		"selected_label", selected,
		"keywords", strings.Join(keywords, ","),
		"sender", msg.Sender,
		"subject", msg.Subject,
	)
	if err := applyKeywordsWithRetry(ctx, p.mail, msg.ID, keywords); err != nil {
		p.log.Error("label apply failed", "message_id", msg.ID, "selected_label", selected, "error", err.Error())
		return err
	}
	p.log.Info("label applied", "message_id", msg.ID, "selected_label", selected, "keywords", strings.Join(keywords, ","))
	if err := p.store.MarkProcessed(msg.ID); err != nil {
		return err
	}
	if err := p.store.AddDecision(state.Decision{
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
	p.maybeSendPushNotification(msg, selected, keywords)
	p.maybeTriggerNovu(msg, selected, keywords)
	return nil
}

func (p *Poller) maybeTriggerNovu(msg imapadapter.Message, selectedLabel string, messageKeywords []string) {
	cfg := p.currentConfig()
	if !shouldSendNotification(cfg.Notifications, selectedLabel, messageKeywords) {
		return
	}
	if p.novu.SecretKey == "" || p.novu.WorkflowID == "" {
		return
	}

	subscriberID, err := p.store.GetOrCreateSubscriberID()
	if err != nil {
		p.log.Error("novu trigger skipped", "message_id", msg.ID, "reason", "subscriber id unavailable", "error", err.Error())
		return
	}

	payload := map[string]any{
		"name":          p.novu.WorkflowID,
		"to":            subscriberID,
		"transactionId": strings.TrimSpace(msg.ID),
		"payload": map[string]any{
			"messageId": strings.TrimSpace(msg.ID),
			"sender":    strings.TrimSpace(msg.Sender),
			"subject":   strings.TrimSpace(msg.Subject),
			"label":     strings.TrimSpace(selectedLabel),
			"keywords":  messageKeywords,
			"url":       "/read",
		},
		"overrides": map[string]any{
			"fcm": map[string]any{
				"data": map[string]any{
					"messageId": strings.TrimSpace(msg.ID),
					"sender":    strings.TrimSpace(msg.Sender),
					"subject":   strings.TrimSpace(msg.Subject),
					"label":     strings.TrimSpace(selectedLabel),
					"keywords":  strings.Join(messageKeywords, ","),
					"url":       "/read",
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("novu trigger skipped", "message_id", msg.ID, "reason", "marshal failed", "error", err.Error())
		return
	}

	req, err := http.NewRequest(http.MethodPost, p.novu.APIBase+"/v1/events/trigger", bytes.NewReader(body))
	if err != nil {
		p.log.Error("novu trigger failed", "message_id", msg.ID, "error", err.Error())
		return
	}
	req.Header.Set("Authorization", "ApiKey "+p.novu.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", strings.TrimSpace(msg.ID))

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		p.log.Error("novu trigger failed", "message_id", msg.ID, "error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		p.log.Error("novu trigger failed", "message_id", msg.ID, "status", strconv.Itoa(resp.StatusCode), "response", strings.TrimSpace(string(bodyBytes)))
		return
	}

	p.log.Info("novu trigger sent", "message_id", msg.ID, "subscriber_id", subscriberID)
}

func (p *Poller) maybeSendPushNotification(msg imapadapter.Message, selectedLabel string, messageKeywords []string) {
	cfg := p.currentConfig()
	if !shouldSendNotification(cfg.Notifications, selectedLabel, messageKeywords) {
		p.log.Info(
			"new-email push notification skipped",
			"reason", "notification mode/keywords did not match",
			"message_id", msg.ID,
			"mode", strings.ToLower(strings.TrimSpace(cfg.Notifications.Mode)),
			"selected_label", strings.TrimSpace(selectedLabel),
			"message_keywords", strings.Join(messageKeywords, ","),
			"configured_keywords", strings.Join(cfg.Notifications.Keywords, ","),
		)
		return
	}

	subs := p.store.ListNotificationSubscriptions()
	if len(subs) == 0 {
		p.log.Info(
			"new-email push notification skipped",
			"reason", "no active push subscriptions",
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
		ok, err := p.store.RemoveNotificationSubscription(endpoint)
		if err == nil && ok {
			removed++
		}
	}

	p.log.Info(
		"new-email push notification attempt",
		"message_id", msg.ID,
		"subscriptions", strconv.Itoa(len(subs)),
		"sent", strconv.Itoa(sent),
		"failed", strconv.Itoa(failed),
		"removed_stale", strconv.Itoa(removed),
	)
}

func shouldSendNotification(settings config.NotificationSettings, selectedLabel string, messageKeywords []string) bool {
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

func classifyWithRetry(ctx context.Context, c llama.Client, labels []string, sender, subject, body string) (string, error) {
	var out string
	var err error
	for i := 0; i < 3; i++ {
		out, err = c.Classify(ctx, labels, sender, subject, body)
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
	newly, err := p.store.SetAICreditsExhausted(now)
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
	if exhausted, _ := p.store.AICreditsExhausted(); !exhausted {
		return
	}
	cleared, err := p.store.ClearAICreditsExhausted()
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

func (p *Poller) allowByRate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.currentConfig()
	now := time.Now()
	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	trimmed := make([]time.Time, 0, len(p.processed))
	for _, t := range p.processed {
		if t.After(hourCutoff) {
			trimmed = append(trimmed, t)
		}
	}
	p.processed = trimmed
	minuteCount := 0
	for _, t := range p.processed {
		if t.After(minuteCutoff) {
			minuteCount++
		}
	}
	if minuteCount >= cfg.RateLimits.PerMinute {
		return false
	}
	if len(p.processed) >= cfg.RateLimits.PerHour {
		return false
	}
	p.processed = append(p.processed, now)
	return true
}
