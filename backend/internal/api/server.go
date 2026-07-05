package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
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
	"llama-lab/backend/internal/fsutil"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/processor"
	"llama-lab/backend/internal/state"
	"llama-lab/backend/internal/users"

	goimap "github.com/BrianLeishman/go-imap"
	"github.com/SherClockHolmes/webpush-go"
)

// Session tracks who a live session token belongs to. Role is deliberately
// not stored here: currentUser looks the user up live from the users store
// on every request so a role change or deactivation take effect on the very
// next request rather than only at next login.
type Session struct {
	UserID    string
	ExpiresAt time.Time
}

// AuthContext identifies the caller of an authenticated request.
type AuthContext struct {
	UserID   string
	Username string
	Role     users.Role
}

type Server struct {
	mu                sync.RWMutex
	cfg               config.Config
	onConfigUpdated   func(config.Config)
	logger            *logging.Logger
	health            *health.Service
	users             *users.Store
	configDir         string
	stateDir          string
	configPath        string
	logPath           string
	imapConfigKeyPath string
	sessions          map[string]Session
	pairingSecret     string
	serverBaseURL     string
	nativeSenders     []processor.NativeSender

	// Per-user resources, lazily created and cached. userMu also guards the
	// subscriberID -> userID index used by the unauthenticated native
	// pairing registration endpoint.
	userMu     sync.Mutex
	userStores map[string]*state.Store
	userMail   map[string]*serverMailEntry
	subIndex   map[string]string
}

func NewServer(cfg config.Config, logger *logging.Logger, healthSvc *health.Service, usersStore *users.Store, onConfigUpdated func(config.Config)) *Server {
	configDir := envOrDefault("CONFIG_DIR", "/llama_lab/config")
	stateDir := envOrDefault("STATE_DIR", "/llama_lab/state")
	logPath := filepath.Join(envOrDefault("LOG_DIR", "/llama_lab/logs"), "app.log")
	imapConfigKeyPath := envOrDefault("IMAP_CONFIG_KEY_FILE", "/llama_lab/private/imap-config.key")
	pairingSecret := strings.TrimSpace(os.Getenv("PAIRING_SECRET"))
	return &Server{
		cfg:               cfg,
		onConfigUpdated:   onConfigUpdated,
		logger:            logger,
		health:            healthSvc,
		users:             usersStore,
		configDir:         configDir,
		stateDir:          stateDir,
		configPath:        filepath.Join(configDir, "config.yaml"),
		logPath:           logPath,
		imapConfigKeyPath: imapConfigKeyPath,
		sessions:          map[string]Session{},
		pairingSecret:     pairingSecret,
		serverBaseURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("SERVER_BASE_URL")), "/"),
		nativeSenders:     processor.NewNativeSendersFromEnv(logger),
		userStores:        map[string]*state.Store{},
		userMail:          map[string]*serverMailEntry{},
		subIndex:          map[string]string{},
	}
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("POST /api/health/repair", s.withAdmin(s.handleRepair))
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	mux.HandleFunc("POST /api/auth/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("POST /api/auth/password", s.withAuth(s.handleChangePassword))
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("PUT /api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("GET /api/labels", s.withAuth(s.handleLabels))
	mux.HandleFunc("GET /api/decisions", s.withAuth(s.handleDecisions))
	mux.HandleFunc("GET /api/inbox", s.withAuth(s.handleInbox))
	mux.HandleFunc("GET /api/inbox/folders", s.withAuth(s.handleInboxFolders))
	mux.HandleFunc("POST /api/inbox/folders", s.withAuth(s.handleInboxFolders))
	mux.HandleFunc("PUT /api/inbox/folders", s.withAuth(s.handleInboxFolders))
	mux.HandleFunc("DELETE /api/inbox/folders", s.withAuth(s.handleInboxFolders))
	mux.HandleFunc("POST /api/inbox/actions", s.withAuth(s.handleInboxActions))
	mux.HandleFunc("GET /api/logs", s.withAdmin(s.handleLogs))
	mux.HandleFunc("GET /api/logs/list", s.withAdmin(s.handleLogsList))
	mux.HandleFunc("GET /api/users", s.withAdmin(s.handleUsersList))
	mux.HandleFunc("POST /api/users", s.withAdmin(s.handleUsersCreate))
	mux.HandleFunc("PUT /api/users/{id}", s.withAdmin(s.handleUsersUpdate))
	mux.HandleFunc("POST /api/users/{id}/reset-password", s.withAdmin(s.handleUsersResetPassword))
	mux.HandleFunc("POST /api/users/{id}/deactivate", s.withAdmin(s.handleUsersDeactivate))
	mux.HandleFunc("POST /api/users/{id}/reactivate", s.withAdmin(s.handleUsersReactivate))
	mux.HandleFunc("GET /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("POST /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("DELETE /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("POST /api/imap/test", s.withAuth(s.handleIMAPTest))
	mux.HandleFunc("POST /api/mail/draft", s.withAuth(s.handleMailDraft))
	mux.HandleFunc("POST /api/mail/send", s.withAuth(s.handleMailSend))
	mux.HandleFunc("POST /api/llama/test", s.withAuth(s.handleLlamaTest))
	mux.HandleFunc("GET /api/tuning", s.withAuth(s.handleTuning))
	mux.HandleFunc("PUT /api/tuning", s.withAuth(s.handleTuning))
	mux.HandleFunc("GET /api/notifications/preferences", s.withAuth(s.handleNotificationPreferences))
	mux.HandleFunc("PUT /api/notifications/preferences", s.withAuth(s.handleNotificationPreferences))
	mux.HandleFunc("GET /api/notifications/vapid-public-key", s.withAuth(s.handleNotificationVAPIDPublicKey))
	mux.HandleFunc("POST /api/notifications/subscriptions", s.withAuth(s.handleNotificationSubscriptions))
	mux.HandleFunc("DELETE /api/notifications/subscriptions", s.withAuth(s.handleNotificationSubscriptions))
	mux.HandleFunc("POST /api/notifications/test", s.withAuth(s.handleNotificationTest))
	mux.HandleFunc("GET /api/notifications/pairing", s.withAuth(s.handleNotificationPairing))
	mux.HandleFunc("POST /api/notifications/native/register", s.handleNotificationNativeRegister)
	mux.HandleFunc("GET /api/notifications/native/devices", s.withAuth(s.handleNotificationNativeDevices))
	mux.HandleFunc("DELETE /api/notifications/native/devices", s.withAuth(s.handleNotificationNativeDevices))
	mux.HandleFunc("POST /api/notifications/native/unpair", s.withAuth(s.handleNotificationNativeUnpair))
	mux.HandleFunc("GET /api/setup", s.handleSetup)
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	processedSince := time.Now().UTC().Add(-1 * time.Hour)
	resp := map[string]any{
		"scanIntervalSeconds":     cfg.Scan.IntervalSeconds,
		"rateLimits":              cfg.RateLimits,
		"checkpoint":              store.Checkpoint(),
		"emailsProcessedLastHour": store.ProcessedSince(processedSince),
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
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	imapConfigPath := s.userIMAPConfigPath(ac.UserID)
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := readIMAPConfigPayload(imapConfigPath, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read imap configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false, "path": imapConfigPath, "keyPath": s.imapConfigKeyPath})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":      true,
			"path":            imapConfigPath,
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

		if err := os.MkdirAll(filepath.Dir(imapConfigPath), 0o700); err != nil {
			http.Error(w, "failed to create imap configuration directory", http.StatusInternalServerError)
			return
		}
		if err := writeIMAPConfigPayload(imapConfigPath, s.imapConfigKeyPath, payload); err != nil {
			http.Error(w, "failed to save imap configuration", http.StatusInternalServerError)
			return
		}
		s.invalidateUserMail(ac.UserID)

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"configured":      true,
			"path":            imapConfigPath,
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
		if err := os.Remove(imapConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove imap configuration", http.StatusInternalServerError)
			return
		}
		s.invalidateUserMail(ac.UserID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	}
}

func (s *Server) handleIMAPTest(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req imapConfigPayload
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

	if strings.TrimSpace(req.Host) == "" || strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
		stored, exists, err := readIMAPConfigPayload(s.userIMAPConfigPath(ac.UserID), s.imapConfigKeyPath)
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

type mailRequest struct {
	Subject string
	Body    string
	Mode    string
	To      []string
	CC      []string
	BCC     []string
}

// decodeMailRequest decodes and validates the shared to/cc/bcc/subject/body/mode
// JSON body used by both the send and draft-save endpoints. On error it returns
// the client-facing error message alongside the error.
func decodeMailRequest(r *http.Request) (mailRequest, string, error) {
	var raw struct {
		To      string `json:"to"`
		CC      string `json:"cc"`
		BCC     string `json:"bcc"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Mode    string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&raw); err != nil {
		return mailRequest{}, "invalid request", err
	}

	toList, err := parseRecipientList(raw.To)
	if err != nil || len(toList) == 0 {
		if err == nil {
			err = errors.New("missing to recipient")
		}
		return mailRequest{}, "valid TO recipient is required", err
	}
	ccList, err := parseRecipientList(raw.CC)
	if err != nil {
		return mailRequest{}, "invalid CC recipients", err
	}
	bccList, err := parseRecipientList(raw.BCC)
	if err != nil {
		return mailRequest{}, "invalid BCC recipients", err
	}

	return mailRequest{
		Subject: raw.Subject,
		Body:    raw.Body,
		Mode:    raw.Mode,
		To:      toList,
		CC:      ccList,
		BCC:     bccList,
	}, "", nil
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

func smtpSendWithImplicitTLS(host string, port int, username, password, from string, recipients []string, msg []byte, timeout time.Duration) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()

	if ok, _ := client.Extension("AUTH"); ok {
		auth := smtp.PlainAuth("", username, password, host)
		if err := client.Auth(auth); err != nil {
			return err
		}
	}

	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(msg); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	if err := client.Quit(); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleMailSend(w http.ResponseWriter, r *http.Request) {
	req, errMsg, err := decodeMailRequest(r)
	if err != nil {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	toList, ccList, bccList := req.To, req.CC, req.BCC

	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	payload, exists, err := readIMAPConfigPayload(s.userIMAPConfigPath(ac.UserID), s.imapConfigKeyPath)
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

	var sendErr error
	if smtpPort == 465 {
		sendErr = smtpSendWithImplicitTLS(smtpHost, smtpPort, payload.Username, payload.Password, from, recipients, msg.Bytes(), 45*time.Second)
	} else {
		auth := smtp.PlainAuth("", payload.Username, payload.Password, smtpHost)
		sendErr = smtpSendWithTimeout(addr, auth, from, recipients, msg.Bytes(), 45*time.Second)
	}
	if sendErr != nil {
		s.logger.Error("mail send failed", "smtpHost", smtpHost, "smtpPort", strconv.Itoa(smtpPort), "error", sendErr.Error())
		http.Error(w, fmt.Sprintf("failed to send email: %s", sendErr.Error()), http.StatusBadGateway)
		return
	}

	warning := ""
	sentSaved := true
	if mailClient, mailErr := s.userMailClient(ac.UserID); mailErr == nil {
		if err := mailClient.SaveSent(r.Context(), imapadapter.DraftMessage{
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
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required before saving drafts", http.StatusBadRequest)
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	req, errMsg, err := decodeMailRequest(r)
	if err != nil {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	if err := mailClient.SaveDraft(r.Context(), imapadapter.DraftMessage{
		To:      req.To,
		CC:      req.CC,
		BCC:     req.BCC,
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
	return normalizeIMAPPayload(payload), true, nil
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
		s.mu.RLock()
		llamaChanged := next.Llama != s.cfg.Llama
		// VAPID key material is server-owned and json:"-" on the wire;
		// carry it across the round-trip.
		next.Notifications = s.cfg.Notifications
		s.mu.RUnlock()
		// Remote LLM settings are admin-only. Reject (rather than silently
		// drop) a non-admin change so a broken save is never masked.
		if ac, ok := authFromContext(r); llamaChanged && (!ok || ac.Role != users.RoleAdmin) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "remote llm settings require admin access"})
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
	}
}

// handleNotificationPreferences reads/writes the calling user's delivery
// preferences (mode/keywords), which moved out of the global config.
func (s *Server) handleNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	path := s.userSettingsPath(ac.UserID)
	switch r.Method {
	case http.MethodGet:
		settings, err := config.LoadUserSettings(path)
		if err != nil {
			http.Error(w, "failed to read notification preferences", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, settings.Notifications)
	case http.MethodPut:
		var prefs config.UserNotificationSettings
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&prefs); err != nil {
			http.Error(w, "invalid preferences payload", http.StatusBadRequest)
			return
		}
		settings, err := config.LoadUserSettings(path)
		if err != nil {
			settings = config.DefaultUserSettings()
		}
		if prefs.Keywords == nil {
			prefs.Keywords = []string{}
		}
		settings.Notifications = prefs
		if err := config.SaveUserSettings(path, settings); err != nil {
			http.Error(w, "failed to save notification preferences", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

type notificationSubscriptionPayload struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		Auth   string `json:"auth"`
		P256DH string `json:"p256dh"`
	} `json:"keys"`
}

type notificationTestPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (s *Server) handleNotificationVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	publicKey := strings.TrimSpace(s.cfg.Notifications.PublicKey)
	s.mu.RUnlock()
	if publicKey == "" {
		http.Error(w, "notification public key not configured", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": publicKey})
}

func (s *Server) handleNotificationSubscriptions(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var payload notificationSubscriptionPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid subscription payload", http.StatusBadRequest)
			return
		}
		payload.Endpoint = strings.TrimSpace(payload.Endpoint)
		payload.Keys.Auth = strings.TrimSpace(payload.Keys.Auth)
		payload.Keys.P256DH = strings.TrimSpace(payload.Keys.P256DH)
		if payload.Endpoint == "" || payload.Keys.Auth == "" || payload.Keys.P256DH == "" {
			http.Error(w, "endpoint and keys are required", http.StatusBadRequest)
			return
		}

		sub := state.NotificationSubscription{
			Endpoint:  payload.Endpoint,
			Auth:      payload.Keys.Auth,
			P256DH:    payload.Keys.P256DH,
			UserAgent: strings.TrimSpace(r.Header.Get("User-Agent")),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := store.UpsertNotificationSubscription(sub); err != nil {
			http.Error(w, "failed to persist notification subscription", http.StatusInternalServerError)
			return
		}
		count := len(store.ListNotificationSubscriptions())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "subscriptions": count})
	case http.MethodDelete:
		var payload struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid unsubscribe payload", http.StatusBadRequest)
			return
		}
		endpoint := strings.TrimSpace(payload.Endpoint)
		if endpoint == "" {
			http.Error(w, "endpoint is required", http.StatusBadRequest)
			return
		}
		removed, err := store.RemoveNotificationSubscription(endpoint)
		if err != nil {
			http.Error(w, "failed to remove notification subscription", http.StatusInternalServerError)
			return
		}
		count := len(store.ListNotificationSubscriptions())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "subscriptions": count})
	}
}

func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	var payload notificationTestPayload
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload)
	title := strings.TrimSpace(payload.Title)
	body := strings.TrimSpace(payload.Body)
	if title == "" {
		title = "Llama Mail Test Notification"
	}
	if body == "" {
		body = "Push delivery is working across all subscribed devices."
	}

	message := map[string]any{
		"title": title,
		"body":  body,
		"url":   "/notifications",
		"tag":   "llama-mail-test",
	}
	payloadBytes, err := json.Marshal(message)
	if err != nil {
		http.Error(w, "failed to serialize notification payload", http.StatusInternalServerError)
		return
	}

	subs := store.ListNotificationSubscriptions()
	sent := 0
	failed := 0
	removed := 0
	if len(subs) > 0 {
		privateKey, err := loadVAPIDPrivateKey(s.cfg.Notifications.PrivateKeyPath)
		if err != nil {
			http.Error(w, "failed to load notification private key", http.StatusInternalServerError)
			return
		}

		options := &webpush.Options{
			Subscriber:      "mailto:noreply@localhost",
			VAPIDPublicKey:  s.cfg.Notifications.PublicKey,
			VAPIDPrivateKey: privateKey,
			TTL:             3600,
		}

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

		for _, endpoint := range staleEndpoints {
			ok, err := store.RemoveNotificationSubscription(endpoint)
			if err == nil && ok {
				removed++
			}
		}
	}

	nativeDevices := store.ListNativeDevices()
	nativeSent := 0
	nativeFailed := 0
	nativeRemoved := 0
	nativeError := ""
	if len(nativeDevices) > 0 && len(s.nativeSenders) == 0 {
		nativeError = "no native push sender configured on the server (set PUSH_RELAY_URL and PUSH_RELAY_KEY)"
		s.logger.Error("test native notification skipped", "reason", nativeError, "devices", strconv.Itoa(len(nativeDevices)))
	} else if len(nativeDevices) > 0 {
		nativeMessage := processor.NativePushMessage{
			Title: title,
			Body:  body,
			Data:  map[string]string{"url": "/notifications"},
		}
		for _, device := range nativeDevices {
			sender := processor.SelectNativeSender(s.nativeSenders, device.Platform)
			if sender == nil {
				nativeFailed++
				s.logger.Error("test native notification failed", "device_id", strings.TrimSpace(device.DeviceID), "platform", strings.TrimSpace(device.Platform), "error", "no sender for platform")
				continue
			}
			sendCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			err := sender.Send(sendCtx, device, nativeMessage)
			cancel()
			if err != nil {
				nativeFailed++
				if errors.Is(err, processor.ErrNativeDeviceStale) && strings.TrimSpace(device.DeviceID) != "" {
					if ok, rmErr := store.RemoveNativeDevice(device.DeviceID); rmErr == nil && ok {
						nativeRemoved++
					}
				}
				s.logger.Error("test native notification failed", "device_id", strings.TrimSpace(device.DeviceID), "platform", strings.TrimSpace(device.Platform), "sender", sender.Name(), "error", err.Error())
				continue
			}
			nativeSent++
		}
	}

	resp := map[string]any{
		"ok":                  failed == 0 && nativeFailed == 0 && nativeError == "",
		"subscriptions":       len(subs),
		"sent":                sent,
		"failed":              failed,
		"removedStale":        removed,
		"activeSubscriptions": len(store.ListNotificationSubscriptions()),
		"nativeDevices":       len(nativeDevices),
		"nativeSent":          nativeSent,
		"nativeFailed":        nativeFailed,
		"nativeRemovedStale":  nativeRemoved,
	}
	if nativeError != "" {
		resp["nativeError"] = nativeError
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNotificationPairing(w http.ResponseWriter, r *http.Request) {
	ac, okAuth := authFromContext(r)
	if !okAuth {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	store, err := s.userStore(ac.UserID)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	subscriberID, err := store.GetOrCreateSubscriberID()
	if err != nil {
		http.Error(w, "failed to load subscriber id", http.StatusInternalServerError)
		return
	}
	// Keep the unauthenticated register endpoint's subscriber -> user index
	// warm so a device pairing right after this call resolves immediately.
	s.userMu.Lock()
	s.subIndex[subscriberID] = ac.UserID
	s.userMu.Unlock()
	configured := s.pairingSecret != ""
	configurationError := ""
	if !configured {
		configurationError = "pairing is not configured on the server; set PAIRING_SECRET"
	}
	serverBaseURL := s.serverBaseURL
	if serverBaseURL == "" {
		serverBaseURL = externalBaseURL(r)
	}
	registerEndpoint := ""
	if serverBaseURL != "" {
		registerEndpoint = strings.TrimRight(serverBaseURL, "/") + "/api/notifications/native/register"
	}
	pairingTTLSeconds := int64(90)
	resp := map[string]any{
		"subscriberId":      subscriberID,
		"serverBaseUrl":     serverBaseURL,
		"registerEndpoint":  registerEndpoint,
		"pairingTtlSeconds": pairingTTLSeconds,
		"configured":        configured,
	}
	if configurationError != "" {
		resp["configurationError"] = configurationError
	}
	if configured {
		resp["subscriberHash"] = s.pairingSubscriberHash(subscriberID)
		token, expiresAt, err := s.createPairingToken(subscriberID, time.Duration(pairingTTLSeconds)*time.Second)
		if err != nil {
			s.logger.Error("failed to create pairing token", "subscriber_id", subscriberID, "error", err.Error())
			http.Error(w, "failed to prepare mobile pairing", http.StatusInternalServerError)
			return
		}
		resp["pairingToken"] = token
		resp["pairingExpiresAt"] = expiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

type nativeRegisterRequest struct {
	SubscriberID   string `json:"subscriberId"`
	SubscriberHash string `json:"subscriberHash"`
	PairingToken   string `json:"pairingToken"`
	DeviceToken    string `json:"deviceToken"`
	DeviceID       string `json:"deviceId,omitempty"`
	Platform       string `json:"platform,omitempty"`
	DeviceName     string `json:"deviceName,omitempty"`
	AppVersion     string `json:"appVersion,omitempty"`
}

func (s *Server) handleNotificationNativeRegister(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}

	var req nativeRegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	subscriberID := strings.TrimSpace(req.SubscriberID)
	subscriberHash := strings.ToLower(strings.TrimSpace(req.SubscriberHash))
	pairingToken := strings.TrimSpace(req.PairingToken)
	deviceToken := strings.TrimSpace(req.DeviceToken)
	if subscriberID == "" || pairingToken == "" || deviceToken == "" {
		http.Error(w, "subscriberId, pairingToken, and deviceToken are required", http.StatusBadRequest)
		return
	}
	if err := s.validatePairingToken(subscriberID, pairingToken, time.Now().UTC()); err != nil {
		http.Error(w, "invalid or expired pairing token", http.StatusUnauthorized)
		return
	}
	if subscriberHash != "" {
		expectedHash := s.pairingSubscriberHash(subscriberID)
		if subtle.ConstantTimeCompare([]byte(subscriberHash), []byte(expectedHash)) != 1 {
			http.Error(w, "invalid subscriber hash", http.StatusUnauthorized)
			return
		}
	}

	// The pairing token proved this device was handed a QR minted by a
	// signed-in user; resolve which user's device list to write into.
	ownerID, okOwner := s.lookupUserBySubscriber(subscriberID)
	if !okOwner {
		http.Error(w, "unknown subscriber", http.StatusUnauthorized)
		return
	}
	store, err := s.userStore(ownerID)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}

	device := state.NativeDevice{
		DeviceID:   strings.TrimSpace(req.DeviceID),
		Platform:   normalizeNativePlatform(req.Platform),
		PushToken:  deviceToken,
		DeviceName: strings.TrimSpace(req.DeviceName),
		AppVersion: strings.TrimSpace(req.AppVersion),
		UserAgent:  strings.TrimSpace(r.Header.Get("User-Agent")),
	}
	if err := store.UpsertNativeDevice(device); err != nil {
		http.Error(w, "failed to persist native device", http.StatusInternalServerError)
		return
	}

	devices := store.ListNativeDevices()
	registeredDeviceID := device.DeviceID
	if registeredDeviceID == "" {
		for i := len(devices) - 1; i >= 0; i-- {
			if strings.TrimSpace(devices[i].PushToken) == deviceToken {
				registeredDeviceID = devices[i].DeviceID
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"synced":   true,
		"deviceId": registeredDeviceID,
		"devices":  len(devices),
	})
}

func (s *Server) handleNotificationNativeDevices(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"devices": store.ListNativeDevices()})
	case http.MethodDelete:
		var payload struct {
			DeviceID string `json:"deviceId"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		deviceID := strings.TrimSpace(payload.DeviceID)
		if deviceID == "" {
			http.Error(w, "deviceId is required", http.StatusBadRequest)
			return
		}
		removed, err := store.RemoveNativeDevice(deviceID)
		if err != nil {
			http.Error(w, "failed to remove native device", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "devices": len(store.ListNativeDevices())})
	}
}

func normalizeNativePlatform(platform string) string {
	clean := strings.ToLower(strings.TrimSpace(platform))
	switch clean {
	case "ios", "android":
		return clean
	default:
		return "android"
	}
}

func (s *Server) handleNotificationNativeUnpair(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	devices := store.ListNativeDevices()
	removed := 0
	for _, device := range devices {
		if strings.TrimSpace(device.DeviceID) == "" {
			continue
		}
		ok, err := store.RemoveNativeDevice(device.DeviceID)
		if err != nil {
			http.Error(w, "failed to revoke paired devices", http.StatusInternalServerError)
			return
		}
		if ok {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "devices": len(store.ListNativeDevices())})
}

func (s *Server) pairingSubscriberHash(subscriberID string) string {
	mac := hmac.New(sha256.New, []byte(s.pairingSecret))
	mac.Write([]byte(subscriberID))
	return hex.EncodeToString(mac.Sum(nil))
}

type pairingTokenClaims struct {
	Sub   string `json:"sub"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"n"`
}

func (s *Server) createPairingToken(subscriberID string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", time.Time{}, err
	}

	expiresAt := time.Now().UTC().Add(ttl)
	claims := pairingTokenClaims{
		Sub:   strings.TrimSpace(subscriberID),
		Exp:   expiresAt.Unix(),
		Nonce: hex.EncodeToString(nonceBytes),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}

	mac := hmac.New(sha256.New, []byte(s.pairingSecret))
	mac.Write(payload)
	sig := mac.Sum(nil)

	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return token, expiresAt, nil
}

func (s *Server) validatePairingToken(subscriberID, token string, now time.Time) error {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return errors.New("invalid token format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("invalid token payload")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid token signature")
	}

	mac := hmac.New(sha256.New, []byte(s.pairingSecret))
	mac.Write(payload)
	expectedSig := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expectedSig) != 1 {
		return errors.New("signature mismatch")
	}

	var claims pairingTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("invalid token claims")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(claims.Sub)), []byte(strings.TrimSpace(subscriberID))) != 1 {
		return errors.New("subscriber mismatch")
	}
	if claims.Exp <= 0 || now.UTC().Unix() > claims.Exp {
		return errors.New("token expired")
	}

	return nil
}

func externalBaseURL(r *http.Request) string {
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return proto + "://" + host
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

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	writeJSON(w, http.StatusOK, store.Decisions(limit))
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

	mailClient, err := s.mailFor(r)
	if err != nil {
		// No mailbox configured yet — show the empty tab scaffold rather
		// than an error so the page still renders.
		tabs = append(tabs, uncategorizedTab)
		writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
		return
	}

	unread, err := mailClient.ListUnreadMessages(r.Context(), mailbox, limit)
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
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		parent := strings.TrimSpace(r.URL.Query().Get("parent"))

		folders, err := mailClient.ListSubfolders(r.Context(), parent)
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

		folder, err := mailClient.CreateFolder(r.Context(), parent, name)
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

		renamed, err := mailClient.RenameFolder(r.Context(), folder, name)
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
		if err := mailClient.DeleteFolder(r.Context(), folder); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"folder": folder,
			"parent": mailboxParentPath(folder),
		})
	}
}

func (s *Server) handleInboxActions(w http.ResponseWriter, r *http.Request) {
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required", http.StatusBadRequest)
			return
		}
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
		if err := mailClient.ApplyInboxAction(r.Context(), messageID, action, mailbox, targetMailbox); err != nil {
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
	s.mu.RLock()
	configured := append([]string{}, s.cfg.Labels.Allowlist...)
	s.mu.RUnlock()

	imapLabels := []string{}
	if mailClient, err := s.mailFor(r); err == nil {
		found, err := mailClient.ListLabels(r.Context())
		if err == nil {
			imapLabels = found
		}
	}
	sort.Strings(imapLabels)
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "imap": imapLabels})
}

func (s *Server) handleTuning(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	tuningPath := s.userTuningPath(ac.UserID)
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(tuningPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// New users start from the install's default tuning prompt.
				fallback := strings.TrimSpace(llama.LoadTuningText())
				if fallback != "" {
					writeJSON(w, http.StatusOK, map[string]any{"content": fallback, "path": tuningPath})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"content": ""})
				return
			}
			http.Error(w, "failed to read tuning file", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"content": string(b), "path": tuningPath})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(tuningPath), 0o755); err != nil {
			http.Error(w, "failed to create tuning directory", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(tuningPath, []byte(req.Content), 0o600); err != nil {
			http.Error(w, "failed to save tuning file", http.StatusInternalServerError)
			return
		}
		// Tuning is now passed to the model per classify call, so no llama
		// process restart is needed for edits to take effect.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": tuningPath, "restartOk": true, "restartError": ""})
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
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
	all, err := s.users.List()
	if err != nil {
		http.Error(w, "failed to read setup state", http.StatusInternalServerError)
		return
	}
	if len(all) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	admin := firstAdmin(all)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "setup": map[string]any{"admin_user": admin.Username, "must_change_password": admin.MustChangePassword}})
}

// firstAdmin returns the earliest-created active admin, used only for the
// pre-login /api/setup hint (prefilling the login form's username).
func firstAdmin(all []users.User) users.User {
	var best users.User
	for _, u := range all {
		if u.Role != users.RoleAdmin || !u.Active {
			continue
		}
		if best.ID == "" || u.CreatedAt < best.CreatedAt {
			best = u
		}
	}
	return best
}

func (s *Server) handleRepair(w http.ResponseWriter, r *http.Request) {
	s.logger.Error("manual repair requested")
	scheduleContainerRestart(s.logger, "manual repair", 250*time.Millisecond)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "restart requested"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	u, err := s.users.GetByUsername(req.Username)
	if err != nil || !u.Active || !users.VerifyPassword(u, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	token, err := randomToken(24)
	if err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[token] = Session{UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
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
	ac, ok := s.currentUser(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	subscriberID := ""
	if store, err := s.userStore(ac.UserID); err == nil {
		subscriberID, _ = store.GetOrCreateSubscriberID()
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "subscriberId": subscriberID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":      true,
		"userId":             u.ID,
		"username":           u.Username,
		"role":               u.Role,
		"mustChangePassword": u.MustChangePassword,
		"subscriberId":       subscriberID,
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
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
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	if !u.MustChangePassword && !users.VerifyPassword(u, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if u.MustChangePassword && strings.TrimSpace(req.OldPassword) != "" && !users.VerifyPassword(u, req.OldPassword) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if _, err := s.users.SetPassword(u.ID, req.NewPassword, false); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLlamaTest(w http.ResponseWriter, r *http.Request) {
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

	result, err := client.Classify(ctx, allowed, "", "", prompt, tuning)
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
		ac, ok := s.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, ac)))
	}
}

type authContextKey struct{}

// authFromContext retrieves the AuthContext injected by withAuth. It only
// returns ok=false if called on a request that never passed through
// withAuth (a programming error), since withAuth already rejects the
// request before next() runs otherwise.
func authFromContext(r *http.Request) (AuthContext, bool) {
	ac, ok := r.Context().Value(authContextKey{}).(AuthContext)
	return ac, ok
}

// currentUser validates the session cookie and looks the owning user up
// live from the users store (not snapshotted into the session), so a role
// change or deactivation take effect on the request immediately following
// it rather than only at next login.
func (s *Server) currentUser(r *http.Request) (AuthContext, bool) {
	cookie, err := r.Cookie("llama_session")
	if err != nil {
		return AuthContext{}, false
	}

	s.mu.Lock()
	sess, ok := s.sessions[cookie.Value]
	if !ok {
		s.mu.Unlock()
		return AuthContext{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
		return AuthContext{}, false
	}
	// Sliding window session expiry for active users.
	s.sessions[cookie.Value] = Session{UserID: sess.UserID, ExpiresAt: time.Now().Add(24 * time.Hour)}
	s.mu.Unlock()

	u, err := s.users.Get(sess.UserID)
	if err != nil || !u.Active {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
		return AuthContext{}, false
	}
	return AuthContext{UserID: u.ID, Username: u.Username, Role: u.Role}, true
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

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func atomicWritePrivateFile(path string, payload []byte) error {
	return fsutil.AtomicWriteFile(path, payload, 0o600)
}
