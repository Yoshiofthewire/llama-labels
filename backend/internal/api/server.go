package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	imapadapter "llama-lab/backend/internal/adapters/imap"
	"llama-lab/backend/internal/adapters/llama"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/state"

	goimap "github.com/BrianLeishman/go-imap"
	"golang.org/x/crypto/scrypt"
)

type Server struct {
	mu                sync.RWMutex
	cfg               config.Config
	onConfigUpdated   func(config.Config)
	logger            *logging.Logger
	store             *state.Store
	health            *health.Service
	configPath        string
	logPath           string
	adminPath         string
	tuningPath        string
	llamaAuthPath     string
	imapConfigPath    string
	imapConfigKeyPath string
	mail              imapadapter.Client
	sessions          map[string]time.Time
}

func NewServer(cfg config.Config, logger *logging.Logger, store *state.Store, healthSvc *health.Service, mailClient imapadapter.Client, onConfigUpdated func(config.Config)) *Server {
	configPath := filepath.Join(envOrDefault("CONFIG_DIR", "/llama_lab/config"), "config.yaml")
	logPath := filepath.Join(envOrDefault("LOG_DIR", "/llama_lab/logs"), "app.log")
	adminPath := filepath.Join(envOrDefault("CONFIG_DIR", "/llama_lab/config"), "admin.env")
	tuningPath := resolveTuningPath()
	llamaAuthPath := envOrDefault("LLAMA_AUTH_FILE", "/llama_lab/config/llama-auth.json")
	imapConfigPath := envOrDefault("IMAP_CONFIG_FILE", "/llama_lab/private/imap-config.json")
	imapConfigKeyPath := envOrDefault("IMAP_CONFIG_KEY_FILE", "/llama_lab/private/imap-config.key")
	return &Server{cfg: cfg, onConfigUpdated: onConfigUpdated, logger: logger, store: store, health: healthSvc, configPath: configPath, logPath: logPath, adminPath: adminPath, tuningPath: tuningPath, llamaAuthPath: llamaAuthPath, imapConfigPath: imapConfigPath, imapConfigKeyPath: imapConfigKeyPath, mail: mailClient, sessions: map[string]time.Time{}}
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/health/repair", s.withAuth(s.handleRepair))
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/me", s.handleMe)
	mux.HandleFunc("/api/auth/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("/api/auth/password", s.withAuth(s.handleChangePassword))
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("/api/labels", s.withAuth(s.handleLabels))
	mux.HandleFunc("/api/decisions", s.withAuth(s.handleDecisions))
	mux.HandleFunc("/api/inbox", s.withAuth(s.handleInbox))
	mux.HandleFunc("/api/inbox/folders", s.withAuth(s.handleInboxFolders))
	mux.HandleFunc("/api/inbox/actions", s.withAuth(s.handleInboxActions))
	mux.HandleFunc("/api/logs", s.withAuth(s.handleLogs))
	mux.HandleFunc("/api/logs/list", s.withAuth(s.handleLogsList))
	mux.HandleFunc("/api/llama/auth", s.withAuth(s.handleLlamaAuth))
	mux.HandleFunc("/api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("/api/imap/test", s.withAuth(s.handleIMAPTest))
	mux.HandleFunc("/api/mail/draft", s.withAuth(s.handleMailDraft))
	mux.HandleFunc("/api/mail/send", s.withAuth(s.handleMailSend))
	mux.HandleFunc("/api/llama/test", s.withAuth(s.handleLlamaTest))
	mux.HandleFunc("/api/tuning", s.withAuth(s.handleTuning))
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/", s.handleFrontend)

	port := envInt("WEB_PORT", 5866)
	s.logger.Info("api server starting", "port", strconv.Itoa(port))
	return http.ListenAndServe(":"+strconv.Itoa(port), mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	st := s.health.GetStatus()
	status := http.StatusOK
	if !st.Healthy {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, st)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	processedSince := time.Now().UTC().Add(-1 * time.Hour)
	resp := map[string]any{
		"scanIntervalSeconds":     cfg.Scan.IntervalSeconds,
		"rateLimits":              cfg.RateLimits,
		"checkpoint":              s.store.Checkpoint(),
		"emailsProcessedLastHour": s.store.ProcessedSince(processedSince),
		"serverTimeUtc":           time.Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, resp)
}

type imapConfigPayload struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Mailbox   string `json:"mailbox"`
	SMTPHost  string `json:"smtpHost,omitempty"`
	SMTPPort  int    `json:"smtpPort,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// normalizeIMAPPayload applies default values and trimming to IMAP config payload.
func normalizeIMAPPayload(p imapConfigPayload) imapConfigPayload {
	p.Host = strings.TrimSpace(p.Host)
	p.Username = strings.TrimSpace(p.Username)
	p.Password = strings.TrimSpace(p.Password)
	p.Mailbox = strings.TrimSpace(p.Mailbox)
	p.SMTPHost = strings.TrimSpace(p.SMTPHost)
	if p.Port <= 0 {
		p.Port = 993
	}
	if p.Mailbox == "" {
		p.Mailbox = "INBOX"
	}
	if p.SMTPHost != "" && p.SMTPPort <= 0 {
		p.SMTPPort = 587
	}
	return p
}

func (s *Server) handleIMAPConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := readIMAPConfigPayload(s.imapConfigPath, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read imap configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false, "path": s.imapConfigPath, "keyPath": s.imapConfigKeyPath})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":      true,
			"path":            s.imapConfigPath,
			"keyPath":         s.imapConfigKeyPath,
			"host":            payload.Host,
			"port":            payload.Port,
			"username":        payload.Username,
			"mailbox":         payload.Mailbox,
			"smtpHost":        payload.SMTPHost,
			"smtpPort":        payload.SMTPPort,
			"updatedAt":       payload.UpdatedAt,
			"encryptedAtRest": true,
		})
	case http.MethodPost:
		var payload imapConfigPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		payload = normalizeIMAPPayload(payload)
		if payload.Host == "" || payload.Username == "" || payload.Password == "" {
			http.Error(w, "host, username, and password are required", http.StatusBadRequest)
			return
		}
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		if err := os.MkdirAll(filepath.Dir(s.imapConfigPath), 0o700); err != nil {
			http.Error(w, "failed to create imap configuration directory", http.StatusInternalServerError)
			return
		}
		if err := writeIMAPConfigPayload(s.imapConfigPath, s.imapConfigKeyPath, payload); err != nil {
			http.Error(w, "failed to save imap configuration", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"configured":      true,
			"path":            s.imapConfigPath,
			"keyPath":         s.imapConfigKeyPath,
			"host":            payload.Host,
			"port":            payload.Port,
			"username":        payload.Username,
			"mailbox":         payload.Mailbox,
			"smtpHost":        payload.SMTPHost,
			"smtpPort":        payload.SMTPPort,
			"updatedAt":       payload.UpdatedAt,
			"encryptedAtRest": true,
		})
	case http.MethodDelete:
		if err := os.Remove(s.imapConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove imap configuration", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIMAPTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req imapConfigPayload
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

	if strings.TrimSpace(req.Host) == "" || strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
		stored, exists, err := readIMAPConfigPayload(s.imapConfigPath, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to load imap configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "host, username, and password are required (or store IMAP config first)", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Host) == "" {
			req.Host = stored.Host
		}
		if req.Port <= 0 {
			req.Port = stored.Port
		}
		if strings.TrimSpace(req.Username) == "" {
			req.Username = stored.Username
		}
		if strings.TrimSpace(req.Password) == "" {
			req.Password = stored.Password
		}
		if strings.TrimSpace(req.Mailbox) == "" {
			req.Mailbox = stored.Mailbox
		}
	}

	req = normalizeIMAPPayload(req)

	client, err := goimap.New(req.Username, req.Password, req.Host, req.Port)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer client.Close()

	if err := client.SelectFolder(req.Mailbox); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": req.Host, "port": req.Port, "mailbox": req.Mailbox})
}

func parseRecipientList(raw string) ([]string, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(raw, ";", ","))
	if normalized == "" {
		return []string{}, nil
	}
	addresses, err := mail.ParseAddressList(normalized)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if addr == nil {
			continue
		}
		clean := strings.TrimSpace(addr.Address)
		if clean == "" {
			continue
		}
		out = append(out, clean)
	}
	return out, nil
}

func sanitizeHeaderValue(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
}

func deriveSMTPHost(imapHost string) string {
	host := strings.TrimSpace(imapHost)
	if host == "" {
		return ""
	}
	lower := strings.ToLower(host)
	if strings.HasPrefix(lower, "imap.") {
		return "smtp." + host[len("imap."):]
	}
	if strings.Contains(lower, ".imap.") {
		return strings.Replace(host, ".imap.", ".smtp.", 1)
	}
	return host
}

func smtpSendWithTimeout(addr string, auth smtp.Auth, from string, recipients []string, msg []byte, timeout time.Duration) error {
	result := make(chan error, 1)
	go func() {
		result <- smtp.SendMail(addr, auth, from, recipients, msg)
	}()

	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("smtp send timed out after %s", timeout)
	}
}

func (s *Server) handleMailSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		To      string `json:"to"`
		CC      string `json:"cc"`
		BCC     string `json:"bcc"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	toList, err := parseRecipientList(req.To)
	if err != nil || len(toList) == 0 {
		http.Error(w, "valid TO recipient is required", http.StatusBadRequest)
		return
	}
	ccList, err := parseRecipientList(req.CC)
	if err != nil {
		http.Error(w, "invalid CC recipients", http.StatusBadRequest)
		return
	}
	bccList, err := parseRecipientList(req.BCC)
	if err != nil {
		http.Error(w, "invalid BCC recipients", http.StatusBadRequest)
		return
	}

	payload, exists, err := readIMAPConfigPayload(s.imapConfigPath, s.imapConfigKeyPath)
	if err != nil {
		http.Error(w, "failed to read mail credentials", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "imap configuration is required before sending", http.StatusBadRequest)
		return
	}

	smtpHost := strings.TrimSpace(payload.SMTPHost)
	if smtpHost == "" {
		smtpHost = strings.TrimSpace(envOrDefault("SMTP_HOST", ""))
	}
	if smtpHost == "" {
		smtpHost = deriveSMTPHost(payload.Host)
	}
	if smtpHost == "" {
		http.Error(w, "smtp host is not configured", http.StatusBadRequest)
		return
	}
	smtpPort := payload.SMTPPort
	if smtpPort <= 0 {
		smtpPort = envInt("SMTP_PORT", 587)
	}
	if smtpPort <= 0 {
		smtpPort = 587
	}
	addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)

	from := sanitizeHeaderValue(payload.Username)
	if from == "" {
		http.Error(w, "imap username is required for sender", http.StatusBadRequest)
		return
	}

	subject := sanitizeHeaderValue(req.Subject)
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	contentType := "text/plain; charset=UTF-8"
	switch mode {
	case "html":
		contentType = "text/html; charset=UTF-8"
	case "markup":
		contentType = "text/markdown; charset=UTF-8"
	}

	var msg bytes.Buffer
	msg.WriteString("From: " + from + "\r\n")
	msg.WriteString("To: " + strings.Join(toList, ", ") + "\r\n")
	if len(ccList) > 0 {
		msg.WriteString("Cc: " + strings.Join(ccList, ", ") + "\r\n")
	}
	msg.WriteString("Subject: " + subject + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: " + contentType + "\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(req.Body)

	recipients := append([]string{}, toList...)
	recipients = append(recipients, ccList...)
	recipients = append(recipients, bccList...)
	s.logger.Info("mail send requested", "smtpHost", smtpHost, "smtpPort", strconv.Itoa(smtpPort), "recipientCount", strconv.Itoa(len(recipients)))

	auth := smtp.PlainAuth("", payload.Username, payload.Password, smtpHost)
	if err := smtpSendWithTimeout(addr, auth, from, recipients, msg.Bytes(), 20*time.Second); err != nil {
		s.logger.Error("mail send failed", "smtpHost", smtpHost, "smtpPort", strconv.Itoa(smtpPort), "error", err.Error())
		http.Error(w, fmt.Sprintf("failed to send email: %s", err.Error()), http.StatusBadGateway)
		return
	}

	warning := ""
	sentSaved := true
	if s.mail != nil {
		if err := s.mail.SaveSent(r.Context(), imapadapter.DraftMessage{
			To:      toList,
			CC:      ccList,
			BCC:     bccList,
			Subject: req.Subject,
			Body:    req.Body,
			Mode:    req.Mode,
		}); err != nil {
			sentSaved = false
			warning = "email sent but could not be saved to Sent folder"
			s.logger.Error("mail sent but save-sent failed", "error", err.Error())
		}
	}
	s.logger.Info("mail send completed", "sentSaved", strconv.FormatBool(sentSaved))

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentSaved": sentSaved, "warning": warning})
}

func (s *Server) handleMailDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mail == nil {
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		To      string `json:"to"`
		CC      string `json:"cc"`
		BCC     string `json:"bcc"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	toList, err := parseRecipientList(req.To)
	if err != nil || len(toList) == 0 {
		http.Error(w, "valid TO recipient is required", http.StatusBadRequest)
		return
	}
	ccList, err := parseRecipientList(req.CC)
	if err != nil {
		http.Error(w, "invalid CC recipients", http.StatusBadRequest)
		return
	}
	bccList, err := parseRecipientList(req.BCC)
	if err != nil {
		http.Error(w, "invalid BCC recipients", http.StatusBadRequest)
		return
	}

	if err := s.mail.SaveDraft(r.Context(), imapadapter.DraftMessage{
		To:      toList,
		CC:      ccList,
		BCC:     bccList,
		Subject: req.Subject,
		Body:    req.Body,
		Mode:    req.Mode,
	}); err != nil {
		http.Error(w, "failed to save draft", http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func readIMAPConfigPayload(path, keyPath string) (imapConfigPayload, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return imapConfigPayload{}, false, nil
		}
		return imapConfigPayload{}, false, err
	}

	plain, err := decryptEncryptedPayload(b, keyPath)
	if err != nil {
		return imapConfigPayload{}, false, err
	}

	var payload imapConfigPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return imapConfigPayload{}, false, err
	}
	payload.Host = strings.TrimSpace(payload.Host)
	payload.Username = strings.TrimSpace(payload.Username)
	payload.Password = strings.TrimSpace(payload.Password)
	payload.Mailbox = strings.TrimSpace(payload.Mailbox)
	payload.SMTPHost = strings.TrimSpace(payload.SMTPHost)
	if payload.Port <= 0 {
		payload.Port = 993
	}
	if payload.Mailbox == "" {
		payload.Mailbox = "INBOX"
	}
	if payload.SMTPHost != "" && payload.SMTPPort <= 0 {
		payload.SMTPPort = 587
	}
	return payload, true, nil
}

func writeIMAPConfigPayload(path, keyPath string, payload imapConfigPayload) error {
	plain, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeEncryptedPayload(path, keyPath, plain)
}

type encryptedPayload struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func writeEncryptedPayload(path, keyPath string, payload []byte) error {
	key, err := loadOrCreateEncryptionKey(keyPath)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	sealed := gcm.Seal(nil, nonce, payload, nil)
	env := encryptedPayload{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}

	return atomicWritePrivateFile(path, b)
}

func decryptEncryptedPayload(raw []byte, keyPath string) ([]byte, error) {
	var env encryptedPayload
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != 1 || strings.TrimSpace(env.Nonce) == "" || strings.TrimSpace(env.Ciphertext) == "" {
		// Backward-compatibility with plaintext credentials.
		return raw, nil
	}

	key, err := loadOrCreateEncryptionKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

func loadOrCreateEncryptionKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if decErr != nil {
			return nil, decErr
		}
		if len(decoded) != 32 {
			return nil, errors.New("invalid encryption master key length")
		}
		return decoded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	if err := atomicWritePrivateFile(path, encoded); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		cfg := s.cfg
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		var next config.Config
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			http.Error(w, "invalid config payload", http.StatusBadRequest)
			return
		}
		if err := config.Save(s.configPath, next); err != nil {
			http.Error(w, "failed to save config", http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.cfg = next
		s.mu.Unlock()
		if s.onConfigUpdated != nil {
			s.onConfigUpdated(next)
		}
		s.logger.Info("config updated via api")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	writeJSON(w, http.StatusOK, s.store.Decisions(limit))
}

type inboxEmail struct {
	MessageID string `json:"messageId"`
	Sender    string `json:"sender"`
	SentTo    string `json:"sentTo,omitempty"`
	CC        string `json:"cc,omitempty"`
	BCC       string `json:"bcc,omitempty"`
	Subject   string `json:"subject"`
	Body      string `json:"body,omitempty"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	AtUTC     string `json:"atUtc"`
}

type inboxFolder struct {
	Path      string `json:"path"`
	Deletable bool   `json:"deletable"`
}

func mailboxLeaf(path string) string {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return ""
	}
	if idx := strings.LastIndexAny(clean, "/."); idx >= 0 && idx+1 < len(clean) {
		return strings.TrimSpace(clean[idx+1:])
	}
	return clean
}

func mailboxParentPath(path string) string {
	clean := strings.TrimSpace(path)
	idx := strings.LastIndexAny(clean, "/.")
	if idx <= 0 {
		return ""
	}
	return clean[:idx]
}

func isBuiltinMailbox(path string) bool {
	leaf := strings.ToLower(mailboxLeaf(path))
	switch leaf {
	case "inbox", "archive", "drafts", "draft", "sent", "sent items", "spam", "junk", "trash", "deleted items":
		return true
	default:
		return false
	}
}

func toInboxFolders(paths []string) []inboxFolder {
	folders := make([]inboxFolder, 0, len(paths))
	for _, folder := range paths {
		clean := strings.TrimSpace(folder)
		if clean == "" {
			continue
		}
		folders = append(folders, inboxFolder{
			Path:      clean,
			Deletable: mailboxParentPath(clean) != "" && !isBuiltinMailbox(clean),
		})
	}
	return folders
}

func firstMatchingKeyword(keywords []string, allowed []string) string {
	if len(keywords) == 0 || len(allowed) == 0 {
		return ""
	}
	seen := map[string]string{}
	for _, keyword := range keywords {
		clean := strings.TrimSpace(keyword)
		if clean == "" {
			continue
		}
		seen[strings.ToLower(clean)] = clean
	}
	for _, allowedKeyword := range allowed {
		key := strings.ToLower(strings.TrimSpace(allowedKeyword))
		if key == "" {
			continue
		}
		if matched, ok := seen[key]; ok {
			return matched
		}
	}
	return ""
}

func collectAllowedKeywords(cfg config.Config) []string {
	out := []string{}
	seen := map[string]bool{}
	appendKeyword := func(value string) {
		clean := strings.TrimSpace(value)
		if clean == "" {
			return
		}
		key := strings.ToLower(clean)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, clean)
	}

	for _, label := range cfg.Labels.Allowlist {
		appendKeyword(label)
	}
	for _, mappedKeywords := range cfg.Labels.KeywordMappings {
		for _, keyword := range mappedKeywords {
			appendKeyword(keyword)
		}
	}
	return out
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 5000 {
			limit = v
		}
	}
	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	allowedKeywords := collectAllowedKeywords(cfg)

	tabs := make([]string, 0, len(allowedKeywords)+1)
	byTab := map[string][]inboxEmail{}
	seenTab := map[string]bool{}

	for _, keyword := range allowedKeywords {
		name := strings.TrimSpace(keyword)
		if name == "" {
			continue
		}
		if seenTab[strings.ToLower(name)] {
			continue
		}
		seenTab[strings.ToLower(name)] = true
		tabs = append(tabs, name)
		byTab[name] = []inboxEmail{}
	}

	const uncategorizedTab = "Uncategorized"
	byTab[uncategorizedTab] = []inboxEmail{}

	if s.mail == nil {
		tabs = append(tabs, uncategorizedTab)
		writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
		return
	}

	unread, err := s.mail.ListUnreadMessages(r.Context(), mailbox, limit)
	if err != nil {
		http.Error(w, "failed to fetch inbox", http.StatusBadGateway)
		return
	}

	for _, msg := range unread {
		tab := firstMatchingKeyword(msg.Keywords, allowedKeywords)
		if tab == "" {
			tab = uncategorizedTab
		}

		if _, ok := byTab[tab]; !ok {
			byTab[tab] = []inboxEmail{}
			if tab != uncategorizedTab {
				tabs = append(tabs, tab)
			}
		}

		status := strings.TrimSpace(msg.Status)
		if status == "" {
			status = "unread"
		}

		byTab[tab] = append(byTab[tab], inboxEmail{
			MessageID: msg.MessageID,
			Sender:    msg.Sender,
			SentTo:    msg.SentTo,
			CC:        msg.CC,
			BCC:       msg.BCC,
			Subject:   msg.Subject,
			Body:      msg.Body,
			Label:     tab,
			Status:    status,
			AtUTC:     msg.AtUTC,
		})
	}

	tabs = append(tabs, uncategorizedTab)
	writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
}

func (s *Server) handleInboxFolders(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		parent := strings.TrimSpace(r.URL.Query().Get("parent"))

		folders, err := s.mail.ListSubfolders(r.Context(), parent)
		if err != nil {
			http.Error(w, "failed to fetch inbox folders", http.StatusBadGateway)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"parent":  parent,
			"folders": toInboxFolders(folders),
		})
	case http.MethodPost:
		var req struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		parent := strings.TrimSpace(req.Parent)
		name := strings.TrimSpace(req.Name)
		if name == "" {
			http.Error(w, "folder name is required", http.StatusBadRequest)
			return
		}

		folder, err := s.mail.CreateFolder(r.Context(), parent, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"parent": parent,
			"name":   name,
			"folder": folder,
		})
	case http.MethodPut:
		var req struct {
			Folder string `json:"folder"`
			Name   string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		folder := strings.TrimSpace(req.Folder)
		name := strings.TrimSpace(req.Name)
		if folder == "" || name == "" {
			http.Error(w, "folder and name are required", http.StatusBadRequest)
			return
		}
		if isBuiltinMailbox(folder) {
			http.Error(w, "built-in folders cannot be renamed", http.StatusBadRequest)
			return
		}
		if mailboxParentPath(folder) == "" {
			http.Error(w, "folder must have a parent mailbox", http.StatusBadRequest)
			return
		}

		renamed, err := s.mail.RenameFolder(r.Context(), folder, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"folder":  folder,
			"renamed": renamed,
			"parent":  mailboxParentPath(renamed),
		})
	case http.MethodDelete:
		folder := strings.TrimSpace(r.URL.Query().Get("folder"))
		if folder == "" {
			http.Error(w, "folder is required", http.StatusBadRequest)
			return
		}
		if isBuiltinMailbox(folder) {
			http.Error(w, "built-in folders cannot be deleted", http.StatusBadRequest)
			return
		}
		if mailboxParentPath(folder) == "" {
			http.Error(w, "folder must have a parent mailbox", http.StatusBadRequest)
			return
		}
		if err := s.mail.DeleteFolder(r.Context(), folder); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"folder": folder,
			"parent": mailboxParentPath(folder),
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleInboxActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mail == nil {
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Action        string   `json:"action"`
		MessageIDs    []string `json:"messageIds"`
		Mailbox       string   `json:"mailbox"`
		TargetMailbox string   `json:"targetMailbox"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	mailbox := strings.TrimSpace(req.Mailbox)
	targetMailbox := strings.TrimSpace(req.TargetMailbox)
	switch action {
	case "delete", "archive", "spam", "read", "move":
	default:
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	if action == "move" && targetMailbox == "" {
		http.Error(w, "targetMailbox is required for move action", http.StatusBadRequest)
		return
	}

	uniqueIDs := make([]string, 0, len(req.MessageIDs))
	seen := map[string]bool{}
	for _, messageID := range req.MessageIDs {
		clean := strings.TrimSpace(messageID)
		if clean == "" {
			continue
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		uniqueIDs = append(uniqueIDs, clean)
	}
	if len(uniqueIDs) == 0 {
		http.Error(w, "at least one messageId is required", http.StatusBadRequest)
		return
	}

	type inboxActionFailure struct {
		MessageID string `json:"messageId"`
		Error     string `json:"error"`
	}
	failures := make([]inboxActionFailure, 0)
	processed := 0
	for _, messageID := range uniqueIDs {
		if err := s.mail.ApplyInboxAction(r.Context(), messageID, action, mailbox, targetMailbox); err != nil {
			failures = append(failures, inboxActionFailure{MessageID: messageID, Error: err.Error()})
			continue
		}
		processed++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            len(failures) == 0,
		"action":        action,
		"processed":     processed,
		"failed":        failures,
		"targetMailbox": targetMailbox,
	})
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	configured := append([]string{}, s.cfg.Labels.Allowlist...)
	s.mu.RUnlock()

	imapLabels := []string{}
	if s.mail != nil {
		found, err := s.mail.ListLabels(r.Context())
		if err == nil {
			imapLabels = found
		}
	}
	sort.Strings(imapLabels)
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "imap": imapLabels})
}

func (s *Server) handleTuning(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(s.tuningPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fallback := strings.TrimSpace(llama.LoadTuningText())
				if fallback != "" {
					writeJSON(w, http.StatusOK, map[string]any{"content": fallback, "path": s.tuningPath})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"content": ""})
				return
			}
			http.Error(w, "failed to read tuning file", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"content": string(b), "path": s.tuningPath})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(s.tuningPath), 0o755); err != nil {
			http.Error(w, "failed to create tuning directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.tuningPath, []byte(req.Content), 0o600); err != nil {
			http.Error(w, "failed to save tuning file", http.StatusInternalServerError)
			return
		}
		restartOk := true
		restartError := ""
		if err := restartLlamaProcess(r.Context()); err != nil {
			restartOk = false
			restartError = err.Error()
		} else {
			llama.ResetWarmupState()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.tuningPath, "restartOk": restartOk, "restartError": restartError})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLlamaAuth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(s.llamaAuthPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusOK, map[string]any{
					"exists":       false,
					"path":         s.llamaAuthPath,
					"localEnabled": strings.EqualFold(envOrDefault("LLAMA_LOCAL_ENABLED", "true"), "true"),
				})
				return
			}
			http.Error(w, "failed to read llama auth status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exists":       true,
			"path":         s.llamaAuthPath,
			"size":         info.Size(),
			"modifiedAt":   info.ModTime().UTC().Format(time.RFC3339),
			"localEnabled": strings.EqualFold(envOrDefault("LLAMA_LOCAL_ENABLED", "true"), "true"),
		})
	case http.MethodPost:
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("authFile")
		if err != nil {
			http.Error(w, "authFile is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		payload, err := io.ReadAll(io.LimitReader(file, 8<<20))
		if err != nil {
			http.Error(w, "failed to read auth file", http.StatusBadRequest)
			return
		}
		if len(strings.TrimSpace(string(payload))) == 0 {
			http.Error(w, "auth file is empty", http.StatusBadRequest)
			return
		}
		var parsed map[string]any
		if err := json.Unmarshal(payload, &parsed); err != nil {
			http.Error(w, "auth file is not valid json", http.StatusBadRequest)
			return
		}
		if len(parsed) == 0 {
			http.Error(w, "auth file json is empty", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(s.llamaAuthPath), 0o755); err != nil {
			http.Error(w, "failed to create auth directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(s.llamaAuthPath, payload, 0o600); err != nil {
			http.Error(w, "failed to save auth file", http.StatusInternalServerError)
			return
		}
		if err := restartLlamaProcess(r.Context()); err != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"ok":           true,
				"path":         s.llamaAuthPath,
				"filename":     header.Filename,
				"restartOk":    false,
				"restartError": err.Error(),
			})
			return
		}
		llama.ResetWarmupState()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"path":      s.llamaAuthPath,
			"filename":  header.Filename,
			"restartOk": true,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 5000 {
			lines = v
		}
	}
	logDir := envOrDefault("LOG_DIR", "/llama_lab/logs")
	// Resolve requested file — default to app.log, allow any *.log in logDir
	filename := filepath.Base(r.URL.Query().Get("file"))
	if filename == "" || filename == "." {
		filename = "app.log"
	}
	// Security: only allow .log files, no path traversal
	if filepath.Ext(filename) != ".log" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.Error(w, "invalid log file", http.StatusBadRequest)
		return
	}
	target := filepath.Join(logDir, filename)
	out, err := tailLines(target, lines)
	if err != nil {
		http.Error(w, "failed to read logs", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": out, "file": filename})
}

func (s *Server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logDir := envOrDefault("LOG_DIR", "/llama_lab/logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		http.Error(w, "failed to list logs", http.StatusInternalServerError)
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			files = append(files, e.Name())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b, err := os.ReadFile(s.adminPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		http.Error(w, "failed to read setup state", http.StatusInternalServerError)
		return
	}
	resp := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		resp[strings.ToLower(parts[0])] = parts[1]
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "setup": map[string]any{"admin_user": resp["admin_user"], "must_change_password": strings.EqualFold(resp["must_change_password"], "true")}})
}

func (s *Server) handleRepair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.logger.Error("manual repair requested")
	scheduleContainerRestart(s.logger, "manual repair", 250*time.Millisecond)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "restart requested"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		http.Error(w, "auth config unavailable", http.StatusInternalServerError)
		return
	}
	if req.Username != admin["ADMIN_USER"] || !verifyAdminPassword(admin, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	token, err := randomToken(24)
	if err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true")})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("llama_session")
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":      true,
		"username":           admin["ADMIN_USER"],
		"mustChangePassword": strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true"),
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username    string `json:"username"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.NewPassword) == "" {
		http.Error(w, "new password required", http.StatusBadRequest)
		return
	}
	admin, err := readAdminEnv(s.adminPath)
	if err != nil {
		http.Error(w, "auth config unavailable", http.StatusInternalServerError)
		return
	}
	mustChange := strings.EqualFold(admin["MUST_CHANGE_PASSWORD"], "true")
	if !mustChange && !verifyAdminPassword(admin, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if mustChange && strings.TrimSpace(req.OldPassword) != "" && !verifyAdminPassword(admin, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	hash, err := hashAdminPassword(req.NewPassword)
	if err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	admin["ADMIN_PASS_HASH"] = hash
	delete(admin, "ADMIN_PASS")
	admin["MUST_CHANGE_PASSWORD"] = "false"
	if err := writeAdminEnv(s.adminPath, admin); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLlamaTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	baseURL := strings.TrimSpace(cfg.Llama.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LLAMA_BASE_URL"))
	}
	if baseURL == "" {
		http.Error(w, "llama base url is not configured", http.StatusBadRequest)
		return
	}

	path := strings.TrimSpace(cfg.Llama.ClassifyPath)
	if path == "" {
		path = "/"
	}
	apiKey := strings.TrimSpace(cfg.Llama.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("LLAMA_API_KEY"))
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "Email Address: test@example.com  Subject Line: Llama connectivity test Return only the label Updates"
	}

	allowed := cfg.Labels.Allowlist
	if len(allowed) == 0 {
		allowed = []string{"Questionable", "Important"}
	}

	tuning := llama.LoadTuningText()
	client := llama.NewHTTPClient(baseURL, apiKey, path, tuning, 120*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	result, err := client.Classify(ctx, allowed, "", "", prompt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"response": result,
		"baseUrl":  baseURL,
		"path":     path,
	})
}

func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	frontendDir := envOrDefault("FRONTEND_DIR", "/opt/llama-lab/frontend")
	indexPath := filepath.Join(frontendDir, "index.html")

	requestPath := path.Clean("/" + r.URL.Path)
	relPath := strings.TrimPrefix(requestPath, "/")

	if relPath != "" {
		assetPath := filepath.Join(frontendDir, relPath)
		rootPrefix := filepath.Clean(frontendDir) + string(os.PathSeparator)
		if strings.HasPrefix(filepath.Clean(assetPath)+string(os.PathSeparator), rootPrefix) {
			if info, err := os.Stat(assetPath); err == nil && !info.IsDir() {
				http.ServeFile(w, r, assetPath)
				return
			}
		}
	}

	if _, err := os.Stat(indexPath); err == nil {
		http.ServeFile(w, r, indexPath)
		return
	}

	http.Error(w, "frontend assets not found; build frontend and set FRONTEND_DIR", http.StatusNotFound)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorize(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) authorize(r *http.Request) bool {
	cookie, err := r.Cookie("llama_session")
	if err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expiresAt) {
		delete(s.sessions, cookie.Value)
		return false
	}

	// Sliding window session expiry for active users.
	s.sessions[cookie.Value] = time.Now().Add(24 * time.Hour)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func scheduleContainerRestart(logger *logging.Logger, reason string, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		if logger != nil {
			logger.Error("container restart requested", "reason", reason)
		}
		_ = syscall.Kill(1, syscall.SIGTERM)
		os.Exit(2)
	}()
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envOrDefault(name, fallback string) string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v
}

func resolveTuningPath() string {
	if envPath := strings.TrimSpace(os.Getenv("TUNING_FILE")); envPath != "" {
		return envPath
	}
	candidates := []string{"/llama_lab/config/TUNING.md", "TUNING.md", "/opt/llama-lab/TUNING.md"}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/llama_lab/config/TUNING.md"
}

func restartProcess(ctx context.Context, processName string, onSuccess func()) error {
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "supervisorctl", args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	out, err := run("-c", "/etc/supervisord.conf", "restart", processName)
	if err == nil {
		if onSuccess != nil {
			onSuccess()
		}
		return nil
	}

	msg := out
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "not running") || strings.Contains(lower, "spawn error") || strings.Contains(lower, "fatal") {
		startOut, startErr := run("-c", "/etc/supervisord.conf", "start", processName)
		if startErr == nil {
			if onSuccess != nil {
				onSuccess()
			}
			return nil
		}
		if strings.TrimSpace(startOut) != "" {
			msg = msg + "; start attempt: " + strings.TrimSpace(startOut)
		}
	}

	return fmt.Errorf("restart %s: %s", processName, msg)
}

func restartLlamaProcess(ctx context.Context) error {
	return restartProcess(ctx, "llama", func() { llama.ResetWarmupState() })
}

func restartDaemonProcess(ctx context.Context) error {
	return restartProcess(ctx, "daemon", nil)
}

// signalDaemonProcessRestart finds the running `llama-lab --mode daemon` process
// and sends it SIGTERM. The daemon program is configured with autorestart=true
// in supervisord, so supervisord respawns it with the freshly written tokens.
// This is used as a fallback when supervisorctl is unavailable, avoiding a full
// container shutdown.
func signalDaemonProcessRestart() error {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("read /proc: %w", err)
	}

	signaled := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		// /proc/<pid>/cmdline is NUL-separated.
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmdline, "llama-lab") {
			continue
		}
		if !strings.Contains(cmdline, "--mode") || !strings.Contains(cmdline, "daemon") {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			signaled++
		}
	}

	if signaled == 0 {
		return errors.New("daemon process not found")
	}
	return nil
}

func tailLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]string, 0, limit)
	s := bufio.NewScanner(f)
	for s.Scan() {
		buf = append(buf, s.Text())
		if len(buf) > limit {
			buf = buf[1:]
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return buf, nil
}

func readAdminEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

func writeAdminEnv(path string, kv map[string]string) error {
	content := fmt.Sprintf("ADMIN_USER=%s\n", kv["ADMIN_USER"])
	if hash := strings.TrimSpace(kv["ADMIN_PASS_HASH"]); hash != "" {
		content += fmt.Sprintf("ADMIN_PASS_HASH=%s\n", hash)
	} else {
		content += fmt.Sprintf("ADMIN_PASS=%s\n", kv["ADMIN_PASS"])
	}
	content += fmt.Sprintf("MUST_CHANGE_PASSWORD=%s\n", kv["MUST_CHANGE_PASSWORD"])
	return os.WriteFile(path, []byte(content), 0o600)
}

func verifyAdminPassword(admin map[string]string, candidate string) bool {
	hash := strings.TrimSpace(admin["ADMIN_PASS_HASH"])
	if hash != "" {
		return verifyScryptHash(hash, candidate)
	}
	legacy := admin["ADMIN_PASS"]
	if legacy == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(legacy), []byte(candidate)) == 1
}

func hashAdminPassword(password string) (string, error) {
	const (
		n      = 16384
		r      = 8
		p      = 1
		keyLen = 32
	)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(password), salt, n, r, p, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"scrypt$%d$%d$%d$%s$%s",
		n,
		r,
		p,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash),
	), nil
}

func verifyScryptHash(encoded, candidate string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	r, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(parts[3])
	if err != nil {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	if len(expected) == 0 {
		return false
	}
	derived, err := scrypt.Key([]byte(candidate), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func atomicWritePrivateFile(path string, payload []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
