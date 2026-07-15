package api

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	"llama-lab/backend/internal/contacts"
	"llama-lab/backend/internal/cryptutil"
	"llama-lab/backend/internal/fsutil"
	"llama-lab/backend/internal/groups"
	"llama-lab/backend/internal/health"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/mailcache"
	"llama-lab/backend/internal/mailmsg"
	"llama-lab/backend/internal/mfa"
	"llama-lab/backend/internal/pgpmail"
	"llama-lab/backend/internal/processor"
	"llama-lab/backend/internal/state"
	"llama-lab/backend/internal/totp"
	"llama-lab/backend/internal/users"

	goimap "github.com/BrianLeishman/go-imap"
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
	mu                   sync.RWMutex
	cfg                  config.Config
	onConfigUpdated      func(config.Config)
	logger               *logging.Logger
	health               *health.Service
	users                *users.Store
	configDir            string
	stateDir             string
	configPath           string
	logPath              string
	imapConfigKeyPath    string
	totpSecretKeyPath    string
	pgpPrivateKeyPath    string
	sessions             map[string]Session
	mfaChallenges        *mfa.Store
	pairingSecret        string
	serverBaseURL        string
	baseURLFallbackWarn  sync.Once
	nativePushDispatcher *processor.NativePushDispatcher
	pickupStore          *pgpmail.PickupStore

	// Per-user resources, lazily created and cached. userMu also guards the
	// subscriberID -> userID index used by the unauthenticated native
	// pairing registration endpoint.
	userMu         sync.Mutex
	userStores     map[string]*state.Store
	userContacts   map[string]*contacts.Store
	userGroups     map[string]*groups.Store
	userMailCache  map[string]*mailcache.Store
	userMail       map[string]*serverMailEntry
	subIndex       map[string]string
	davCredentials davCredentialCache
}

func NewServer(cfg config.Config, logger *logging.Logger, healthSvc *health.Service, usersStore *users.Store, onConfigUpdated func(config.Config)) *Server {
	configDir := config.EnvOrDefault("CONFIG_DIR", "/llama_lab/config")
	stateDir := config.EnvOrDefault("STATE_DIR", "/llama_lab/state")
	logPath := filepath.Join(config.EnvOrDefault("LOG_DIR", "/llama_lab/logs"), "app.log")
	imapConfigKeyPath := config.EnvOrDefault("IMAP_CONFIG_KEY_FILE", "/llama_lab/private/imap-config.key")
	totpSecretKeyPath := config.EnvOrDefault("TOTP_SECRET_KEY_FILE", "/llama_lab/private/totp-secret.key")
	pgpPrivateKeyPath := config.EnvOrDefault("PGP_PRIVATE_KEY_FILE", "/llama_lab/private/pgp-private-key.key")
	pickupStoreKeyPath := config.EnvOrDefault("PICKUP_STORE_KEY_FILE", "/llama_lab/private/pickup-store.key")
	pairingSecret := strings.TrimSpace(os.Getenv("PAIRING_SECRET"))
	return &Server{
		cfg:                  cfg,
		onConfigUpdated:      onConfigUpdated,
		logger:               logger,
		health:               healthSvc,
		users:                usersStore,
		configDir:            configDir,
		stateDir:             stateDir,
		configPath:           filepath.Join(configDir, "config.yaml"),
		logPath:              logPath,
		imapConfigKeyPath:    imapConfigKeyPath,
		totpSecretKeyPath:    totpSecretKeyPath,
		pgpPrivateKeyPath:    pgpPrivateKeyPath,
		sessions:             map[string]Session{},
		mfaChallenges:        mfa.NewStore(),
		pairingSecret:        pairingSecret,
		serverBaseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("SERVER_BASE_URL")), "/"),
		nativePushDispatcher: processor.NewNativePushDispatcher(logger),
		pickupStore:          pgpmail.NewPickupStore(filepath.Join(stateDir, "pickup"), pickupStoreKeyPath),
		userStores:           map[string]*state.Store{},
		userContacts:         map[string]*contacts.Store{},
		userGroups:           map[string]*groups.Store{},
		userMailCache:        map[string]*mailcache.Store{},
		userMail:             map[string]*serverMailEntry{},
		subIndex:             map[string]string{},
		davCredentials:       newDAVCredentialCache(),
	}
}

// routes builds the API's route table. Split out from Run so tests can
// dispatch through the exact same registration (middleware included)
// instead of calling handlers directly and assuming the wiring matches.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("POST /api/health/repair", s.withAdmin(s.handleRepair))
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/mfa/totp", s.handleMFATOTP)
	mux.HandleFunc("POST /api/auth/mfa/recovery-code", s.handleMFARecoveryCode)
	mux.HandleFunc("POST /api/auth/mfa/push/poll", s.handlePushPoll)
	mux.HandleFunc("POST /api/auth/mfa/push/finish", s.handlePushFinish)
	mux.HandleFunc("POST /api/mfa/push/respond", s.handlePushRespond)
	mux.HandleFunc("GET /api/mfa/status", s.withAuth(s.handleMFAStatus))
	mux.HandleFunc("POST /api/mfa/totp/setup", s.withAuth(s.handleMFASetup))
	mux.HandleFunc("POST /api/mfa/totp/confirm", s.withAuth(s.handleMFAConfirm))
	mux.HandleFunc("POST /api/mfa/totp/disable", s.withAuth(s.handleMFADisable))
	mux.HandleFunc("POST /api/mfa/recovery-codes/regenerate", s.withAuth(s.handleMFARecoveryCodesRegenerate))
	mux.HandleFunc("PUT /api/mfa/push/enabled", s.withAuth(s.handleMFAPushEnabled))
	mux.HandleFunc("PUT /api/notifications/native/devices/{deviceId}/mfa", s.withAuth(s.handleNativeDeviceMFA))
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	mux.HandleFunc("POST /api/auth/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("POST /api/auth/password", s.withAuth(s.handleChangePassword))
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("PUT /api/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("GET /api/labels", s.withAuth(s.handleLabels))
	mux.HandleFunc("GET /api/decisions", s.withAuth(s.handleDecisions))
	mux.HandleFunc("GET /api/inbox", s.withMailAuth(s.handleInbox))
	mux.HandleFunc("GET /api/inbox/folders", s.withMailAuth(s.handleInboxFolders))
	mux.HandleFunc("POST /api/inbox/folders", s.withMailAuth(s.handleInboxFolders))
	mux.HandleFunc("PUT /api/inbox/folders", s.withMailAuth(s.handleInboxFolders))
	mux.HandleFunc("DELETE /api/inbox/folders", s.withMailAuth(s.handleInboxFolders))
	mux.HandleFunc("POST /api/inbox/actions", s.withMailAuth(s.handleInboxActions))
	mux.HandleFunc("GET /api/mail/search", s.withMailAuth(s.handleMailSearch))
	mux.HandleFunc("GET /api/logs", s.withAdmin(s.handleLogs))
	mux.HandleFunc("GET /api/logs/list", s.withAdmin(s.handleLogsList))
	mux.HandleFunc("GET /api/users", s.withAdmin(s.handleUsersList))
	mux.HandleFunc("POST /api/users", s.withAdmin(s.handleUsersCreate))
	mux.HandleFunc("PUT /api/users/{id}", s.withAdmin(s.handleUsersUpdate))
	mux.HandleFunc("POST /api/users/{id}/reset-password", s.withAdmin(s.handleUsersResetPassword))
	mux.HandleFunc("POST /api/users/{id}/deactivate", s.withAdmin(s.handleUsersDeactivate))
	mux.HandleFunc("POST /api/users/{id}/reactivate", s.withAdmin(s.handleUsersReactivate))
	mux.HandleFunc("POST /api/users/{id}/clear-mfa", s.withAdmin(s.handleUsersClearMFA))
	mux.HandleFunc("GET /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("POST /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("DELETE /api/imap/config", s.withAuth(s.handleIMAPConfig))
	mux.HandleFunc("POST /api/imap/test", s.withAuth(s.handleIMAPTest))
	mux.HandleFunc("POST /api/mail/draft", s.withMailAuth(s.handleMailDraft))
	mux.HandleFunc("POST /api/mail/send", s.withMailAuth(s.handleMailSend))
	mux.HandleFunc("GET /api/mail/attachments", s.withMailAuth(s.handleMailAttachmentList))
	mux.HandleFunc("GET /api/mail/attachment", s.withMailAuth(s.handleMailAttachmentDownload))
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
	mux.HandleFunc("PUT /api/notifications/native/mode", s.withAuth(s.handleNotificationNativeMode))
	mux.HandleFunc("GET /api/notifications/native/pull", s.handleNotificationNativePull)
	mux.HandleFunc("POST /api/notifications/desktop/pair", s.withAuth(s.handleDesktopPair))
	mux.HandleFunc("GET /api/contacts", s.withAuth(s.handleContacts))
	mux.HandleFunc("POST /api/contacts", s.withAuth(s.handleContacts))
	mux.HandleFunc("POST /api/contacts/dedupe", s.withMailAuth(s.handleContactsDedupe))
	mux.HandleFunc("POST /api/contacts/bulk-delete", s.withAuth(s.handleContactsBulkDelete))
	mux.HandleFunc("GET /api/contacts/export", s.withAuth(s.handleContactsExport))
	mux.HandleFunc("POST /api/contacts/import", s.withAuth(s.handleContactsImport))
	mux.HandleFunc("GET /api/contacts/dav-password", s.withAuth(s.handleContactsDAVPassword))
	mux.HandleFunc("POST /api/contacts/dav-password", s.withAuth(s.handleContactsDAVPassword))
	mux.HandleFunc("DELETE /api/contacts/dav-password", s.withAuth(s.handleContactsDAVPassword))
	mux.HandleFunc("GET /api/contacts/carddav-client/config", s.withAuth(s.handleContactsCardDAVClientConfig))
	mux.HandleFunc("POST /api/contacts/carddav-client/config", s.withAuth(s.handleContactsCardDAVClientConfig))
	mux.HandleFunc("DELETE /api/contacts/carddav-client/config", s.withAuth(s.handleContactsCardDAVClientConfig))
	mux.HandleFunc("POST /api/contacts/carddav-client/sync", s.withAuth(s.handleContactsCardDAVClientSync))
	mux.HandleFunc("GET /api/contacts/{id}", s.withAuth(s.handleContactByID))
	mux.HandleFunc("PUT /api/contacts/{id}", s.withAuth(s.handleContactByID))
	mux.HandleFunc("DELETE /api/contacts/{id}", s.withAuth(s.handleContactByID))
	mux.HandleFunc("GET /api/contacts/sync", s.handleContactsSync)
	mux.HandleFunc("POST /api/contacts/sync", s.handleContactsSync)
	mux.HandleFunc("POST /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
	mux.HandleFunc("GET /api/contacts/{id}/photo", s.withMailAuth(s.handleContactPhoto))
	mux.HandleFunc("DELETE /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
	mux.HandleFunc("POST /api/pgp/identity/generate", s.withAuth(s.handlePGPIdentityGenerate))
	mux.HandleFunc("POST /api/pgp/identity/import", s.withAuth(s.handlePGPIdentityImport))
	mux.HandleFunc("GET /api/pgp/identity", s.withAuth(s.handlePGPIdentity))
	mux.HandleFunc("DELETE /api/pgp/identity", s.withAuth(s.handlePGPIdentity))
	mux.HandleFunc("GET /api/pgp/keyserver/lookup", s.withAuth(s.handlePGPKeyserverLookup))
	mux.HandleFunc("POST /api/pgp/recipients/check", s.withAuth(s.handlePGPRecipientsCheck))
	mux.HandleFunc("GET /api/pgp/qr/token", s.withMailAuth(s.handlePGPQRToken))
	mux.HandleFunc("GET /api/pgp/qr/key", s.handlePGPQRKey)
	mux.HandleFunc("GET /api/groups", s.withMailAuth(s.handleGroups))
	mux.HandleFunc("POST /api/groups", s.withAuth(s.handleGroups))
	mux.HandleFunc("PUT /api/groups/{id}", s.withAuth(s.handleGroupByID))
	mux.HandleFunc("DELETE /api/groups/{id}", s.withAuth(s.handleGroupByID))
	mux.Handle("/.well-known/carddav", s.withDAVBasicAuth(http.HandlerFunc(s.handleCardDAV)))
	mux.Handle(davPrefix+"/", s.withDAVBasicAuth(http.HandlerFunc(s.handleCardDAV)))
	mux.HandleFunc("GET /api/setup", s.handleSetup)
	mux.HandleFunc("GET /pickup/{id}", s.handlePickup)
	mux.HandleFunc("/", s.handleFrontend)

	return mux
}

func (s *Server) Run() error {
	port := envInt("WEB_PORT", 5866)
	s.logger.Info("api server starting", "port", strconv.Itoa(port))
	return http.ListenAndServe(":"+strconv.Itoa(port), s.routes())
}

// StartPickupSweeper runs PickupStore.Sweep on an interval for the process
// lifetime, mirroring processor.Poller's ticker/cancel pattern
// (backend/internal/processor/poller.go). Call once after NewServer, e.g.
// `go srv.StartPickupSweeper(context.Background())` alongside wherever the
// existing background poller is started.
func (s *Server) StartPickupSweeper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.pickupStore.Sweep(30 * 24 * time.Hour); err != nil {
				s.logger.Error("pickup sweep failed", "error", err.Error())
			}
		}
	}
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
	Subject     string
	Body        string
	Mode        string
	To          []string
	CC          []string
	BCC         []string
	Attachments []mailmsg.Attachment
	Encrypt     bool
	Sign        bool
}

// Attachment budget for one outgoing message (decoded bytes); the request
// body limit leaves headroom for the ~4/3 base64 overhead plus the JSON.
const (
	maxMailAttachmentBytes = 25 << 20
	maxMailRequestBytes    = 40 << 20
)

// decodeMailRequest decodes and validates the shared to/cc/bcc/subject/body/
// mode/attachments JSON body used by both the send and draft-save endpoints.
// On error it returns the client-facing error message alongside the error.
func decodeMailRequest(r *http.Request) (mailRequest, string, error) {
	var raw struct {
		To          string `json:"to"`
		CC          string `json:"cc"`
		BCC         string `json:"bcc"`
		Subject     string `json:"subject"`
		Body        string `json:"body"`
		Mode        string `json:"mode"`
		Attachments []struct {
			Name       string `json:"name"`
			MimeType   string `json:"mimeType"`
			DataBase64 string `json:"dataBase64"`
		} `json:"attachments"`
		Encrypt bool `json:"encrypt"`
		Sign    bool `json:"sign"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMailRequestBytes)).Decode(&raw); err != nil {
		return mailRequest{}, "invalid request", err
	}

	attachments := make([]mailmsg.Attachment, 0, len(raw.Attachments))
	attachmentTotal := 0
	for _, a := range raw.Attachments {
		content, err := base64.StdEncoding.DecodeString(a.DataBase64)
		if err != nil {
			return mailRequest{}, "invalid attachment encoding", err
		}
		attachmentTotal += len(content)
		if attachmentTotal > maxMailAttachmentBytes {
			return mailRequest{}, "attachments too large (max 25 MB total)",
				errors.New("attachment size limit exceeded")
		}
		attachments = append(attachments, mailmsg.Attachment{
			Name:     a.Name,
			MimeType: a.MimeType,
			Content:  content,
		})
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
		Subject:     raw.Subject,
		Body:        raw.Body,
		Mode:        raw.Mode,
		To:          toList,
		CC:          ccList,
		BCC:         bccList,
		Attachments: attachments,
		Encrypt:     raw.Encrypt,
		Sign:        raw.Sign,
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

// findContactPGPKey looks up email among the store's contacts (case-
// insensitive) and returns its armored PGP public key, if the matching
// contact has one on file.
func findContactPGPKey(store *contacts.Store, email string) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(email))
	if target == "" {
		return "", false
	}
	for _, c := range store.List() {
		if c.PGPKey == "" {
			continue
		}
		for _, e := range c.Emails {
			if strings.ToLower(strings.TrimSpace(e.Value)) == target {
				return c.PGPKey, true
			}
		}
	}
	return "", false
}

// pgpRecipientPlan splits an encrypted send's To/CC/BCC recipients by PGP
// key availability and status. To/CC recipients with a usable key share one
// ciphertext, matching how a normal email is visible to every To/CC
// recipient. BCC recipients are kept separate so each can be encrypted
// individually in buildPGPDeliveries — sharing a ciphertext (and its
// embedded recipient key IDs) with anyone else would deanonymize them.
// Recipients with no key on file, or whose key is revoked or expired, land
// in withoutKeyEmails and fall back to the existing plaintext pickup-link
// notification.
type pgpRecipientPlan struct {
	toCCEmails       []string
	toCCKeys         []string
	bccEmails        []string
	bccKeys          []string
	withoutKeyEmails []string
}

// buildPGPRecipientPlan resolves each recipient's contact PGP key and
// builds a pgpRecipientPlan. Recipients are deduplicated case-insensitively
// across To+CC+BCC combined, keeping only the first occurrence — an address
// listed in both To and BCC is treated as a To recipient.
func buildPGPRecipientPlan(toList, ccList, bccList []string, contactsStore *contacts.Store) pgpRecipientPlan {
	var plan pgpRecipientPlan
	seen := map[string]bool{}

	resolve := func(recipient string) (armoredKey string, usable bool) {
		key, ok := findContactPGPKey(contactsStore, recipient)
		if !ok {
			return "", false
		}
		status, err := pgpmail.CheckKeyStatus(key)
		if err != nil || !status.Usable() {
			return "", false
		}
		return key, true
	}

	toCC := append(append([]string{}, toList...), ccList...)
	for _, recipient := range toCC {
		lower := strings.ToLower(strings.TrimSpace(recipient))
		if lower == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		if key, ok := resolve(recipient); ok {
			plan.toCCEmails = append(plan.toCCEmails, recipient)
			plan.toCCKeys = append(plan.toCCKeys, key)
		} else {
			plan.withoutKeyEmails = append(plan.withoutKeyEmails, recipient)
		}
	}
	for _, recipient := range bccList {
		lower := strings.ToLower(strings.TrimSpace(recipient))
		if lower == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		if key, ok := resolve(recipient); ok {
			plan.bccEmails = append(plan.bccEmails, recipient)
			plan.bccKeys = append(plan.bccKeys, key)
		} else {
			plan.withoutKeyEmails = append(plan.withoutKeyEmails, recipient)
		}
	}
	return plan
}

// pgpDelivery is one PGP/MIME ciphertext and the SMTP recipient(s) it
// should be delivered to in a single transaction.
type pgpDelivery struct {
	Recipients []string
	Ciphertext []byte
}

// buildPGPDeliveries encrypts msg once for plan's shared To/CC recipients
// (if any) and once individually for each of plan's BCC recipients, so no
// BCC recipient's key ID ever appears in a ciphertext another recipient can
// inspect. signer is passed straight through to EncryptMIME for every
// delivery (nil if the caller didn't request signing).
func buildPGPDeliveries(msg []byte, plan pgpRecipientPlan, signer *pgpmail.Identity) ([]pgpDelivery, error) {
	var deliveries []pgpDelivery
	if len(plan.toCCEmails) > 0 {
		ciphertext, err := pgpmail.EncryptMIME(msg, plan.toCCKeys, signer)
		if err != nil {
			return nil, fmt.Errorf("encrypt to/cc recipients: %w", err)
		}
		deliveries = append(deliveries, pgpDelivery{Recipients: plan.toCCEmails, Ciphertext: ciphertext})
	}
	for i, recipient := range plan.bccEmails {
		ciphertext, err := pgpmail.EncryptMIME(msg, []string{plan.bccKeys[i]}, signer)
		if err != nil {
			return nil, fmt.Errorf("encrypt bcc recipient %s: %w", recipient, err)
		}
		deliveries = append(deliveries, pgpDelivery{Recipients: []string{recipient}, Ciphertext: ciphertext})
	}
	return deliveries, nil
}

// smtpDeliver sends msg over SMTP to recipients, choosing implicit TLS
// (port 465) or STARTTLS/plain auth otherwise. Extracted from
// finishMailSend so per-BCC-recipient encrypted sends (handleMailSend) can
// reuse the same transport logic for their own separate SMTP transactions.
func smtpDeliver(smtpHost string, smtpPort int, addr, smtpUsername, smtpPassword, from string, recipients []string, msg []byte) error {
	if smtpPort == 465 {
		return smtpSendWithImplicitTLS(smtpHost, smtpPort, smtpUsername, smtpPassword, from, recipients, msg, 45*time.Second)
	}
	auth := smtp.PlainAuth("", smtpUsername, smtpPassword, smtpHost)
	return smtpSendWithTimeout(addr, auth, from, recipients, msg, 45*time.Second)
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
		smtpHost = strings.TrimSpace(config.EnvOrDefault("SMTP_HOST", ""))
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

	msg := mailmsg.Message{
		From:        from,
		To:          toList,
		CC:          ccList,
		Subject:     req.Subject,
		Body:        req.Body,
		Mode:        req.Mode,
		Attachments: req.Attachments,
	}.Build()

	var signer *pgpmail.Identity
	if req.Sign || req.Encrypt {
		u, uerr := s.users.Get(ac.UserID)
		if uerr == nil && u.PGPPrivateKeyEnc != "" {
			signer, err = pgpmail.OpenPrivateKey(u.PGPPrivateKeyEnc, s.pgpPrivateKeyPath)
			if err != nil {
				http.Error(w, "failed to load pgp identity", http.StatusInternalServerError)
				return
			}
		} else if req.Sign {
			http.Error(w, "signing requires a pgp identity — generate or import one first", http.StatusBadRequest)
			return
		}
	}
	if req.Sign && signer != nil {
		if status := signer.Status(); !status.Usable() {
			http.Error(w, "cannot sign — your pgp identity is revoked or expired, generate or import a new one", http.StatusBadRequest)
			return
		}
	}

	if !req.Encrypt {
		if req.Sign {
			signed, serr := pgpmail.SignMIME(msg, signer)
			if serr != nil {
				http.Error(w, "failed to sign message", http.StatusInternalServerError)
				return
			}
			msg = signed
		}
		recipients := append(append(append([]string{}, toList...), ccList...), bccList...)
		s.finishMailSend(w, r, ac.UserID, smtpHost, smtpPort, addr, payload.Username, payload.Password, from, toList, ccList, bccList, recipients, msg, req)
		return
	}

	contactsStore, cerr := s.userContactsStore(ac.UserID)
	if cerr != nil {
		http.Error(w, "failed to open contacts store", http.StatusInternalServerError)
		return
	}
	plan := buildPGPRecipientPlan(toList, ccList, bccList, contactsStore)
	if len(plan.toCCEmails) == 0 && len(plan.bccEmails) == 0 {
		http.Error(w, "none of the recipients have a known pgp key — disable encryption or add keys to your contacts first", http.StatusBadRequest)
		return
	}

	deliveries, eerr := buildPGPDeliveries(msg, plan, encryptSigner(signer, req.Sign))
	if eerr != nil {
		http.Error(w, "failed to encrypt message", http.StatusInternalServerError)
		return
	}

	// deliveries[0] is always the correct hard-error-gated send: buildPGPDeliveries
	// guarantees the shared To/CC ciphertext (if any) comes first, otherwise the
	// first BCC recipient's ciphertext is deliveries[0]. deliveries is guaranteed
	// non-empty here because we already returned a 400 above when both
	// plan.toCCEmails and plan.bccEmails were empty. Treating index 0 uniformly
	// (rather than special-casing on len(plan.toCCEmails) > 0) avoids a BCC-only
	// send picking an empty "main" delivery, which previously let finishMailSend
	// report ok:true via its empty-recipient-list guard before any of the actual
	// best-effort BCC sends had even been attempted.
	mainRecipients, mainCiphertext := deliveries[0].Recipients, deliveries[0].Ciphertext
	bccDeliveries := deliveries[1:]

	if !s.finishMailSend(w, r, ac.UserID, smtpHost, smtpPort, addr, payload.Username, payload.Password, from, toList, ccList, bccList, mainRecipients, mainCiphertext, req) {
		return
	}

	for _, delivery := range bccDeliveries {
		if err := smtpDeliver(smtpHost, smtpPort, addr, payload.Username, payload.Password, from, delivery.Recipients, delivery.Ciphertext); err != nil {
			s.logger.Error("bcc pgp send failed", "recipient", delivery.Recipients[0], "error", err.Error())
		}
	}

	for _, recipient := range plan.withoutKeyEmails {
		if err := s.sendPickupNotification(ac.UserID, from, recipient, req.Subject, req.Body, smtpHost, smtpPort, addr, payload.Username, payload.Password); err != nil {
			s.logger.Error("pickup notification send failed", "recipient", recipient, "error", err.Error())
		}
	}
}

// encryptSigner decides which signer identity (if any) should be embedded
// into an encrypted message. Encrypt and Sign are independent per-email
// toggles: an identity being loaded (because Encrypt requires checking
// whether one exists, or because Sign itself was requested) must not imply
// the message gets signed. Only pass a signer through to EncryptMIME when
// the caller explicitly asked to sign — otherwise Encrypt=true, Sign=false
// would silently produce a signed-and-encrypted message whenever the sender
// happens to have a PGP identity configured, costing them deniability they
// never asked to give up.
func encryptSigner(signer *pgpmail.Identity, sign bool) *pgpmail.Identity {
	if !sign {
		return nil
	}
	return signer
}

// finishMailSend sends msg over SMTP to recipients and best-effort saves it
// to the Sent folder (as plaintext — see the plan's Global Constraints on
// why the Sent copy isn't PGP-wrapped), writing the JSON response. Returns
// false if the send itself failed (response already written), so callers
// with follow-up work (e.g. pickup notifications) know not to proceed.
func (s *Server) finishMailSend(w http.ResponseWriter, r *http.Request, userID, smtpHost string, smtpPort int, addr, smtpUsername, smtpPassword, from string, toList, ccList, bccList, recipients []string, msg []byte, req mailRequest) bool {
	s.logger.Info("mail send requested", "smtpHost", smtpHost, "smtpPort", strconv.Itoa(smtpPort), "recipientCount", strconv.Itoa(len(recipients)))

	if len(recipients) > 0 {
		if sendErr := smtpDeliver(smtpHost, smtpPort, addr, smtpUsername, smtpPassword, from, recipients, msg); sendErr != nil {
			s.logger.Error("mail send failed", "smtpHost", smtpHost, "smtpPort", strconv.Itoa(smtpPort), "error", sendErr.Error())
			http.Error(w, fmt.Sprintf("failed to send email: %s", sendErr.Error()), http.StatusBadGateway)
			return false
		}
	}

	warning := ""
	sentSaved := true
	if mailClient, mailErr := s.userMailClient(userID); mailErr == nil {
		if err := mailClient.SaveSent(r.Context(), imapadapter.DraftMessage{
			To:          toList,
			CC:          ccList,
			BCC:         bccList,
			Subject:     req.Subject,
			Body:        req.Body,
			Mode:        req.Mode,
			Attachments: req.Attachments,
		}); err != nil {
			sentSaved = false
			warning = "email sent but could not be saved to Sent folder"
			s.logger.Error("mail sent but save-sent failed", "error", err.Error())
		}
	}
	s.logger.Info("mail send completed", "sentSaved", strconv.FormatBool(sentSaved))

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentSaved": sentSaved, "warning": warning})
	return true
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
		To:          req.To,
		CC:          req.CC,
		BCC:         req.BCC,
		Subject:     req.Subject,
		Body:        req.Body,
		Mode:        req.Mode,
		Attachments: req.Attachments,
	}); err != nil {
		http.Error(w, "failed to save draft", http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// attachmentRequestParams reads the shared mailbox/messageId query params of
// the two attachment endpoints. messageId is an IMAP UID, the same id shape
// /api/inbox and /api/inbox/actions use.
func attachmentRequestParams(r *http.Request) (mailbox string, uid int, err error) {
	mailbox = strings.TrimSpace(r.URL.Query().Get("mailbox"))
	uid, err = strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("messageId")))
	if err != nil || uid <= 0 {
		return "", 0, errors.New("valid messageId is required")
	}
	return mailbox, uid, nil
}

// handleMailAttachmentList returns attachment metadata for one message.
// GET /api/mail/attachments?sub=&hash=&mailbox=&messageId=
func (s *Server) handleMailAttachmentList(w http.ResponseWriter, r *http.Request) {
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}
	s.serveAttachmentList(w, r, mailClient)
}

func (s *Server) serveAttachmentList(w http.ResponseWriter, r *http.Request, mailClient imapadapter.Client) {
	mailbox, uid, err := attachmentRequestParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	infos, err := mailClient.ListAttachments(r.Context(), mailbox, uid)
	if err != nil {
		s.logger.Error("attachment list failed", "mailbox", mailbox, "uid", strconv.Itoa(uid), "error", err.Error())
		http.Error(w, "failed to list attachments", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "attachments": infos})
}

// handleMailAttachmentDownload streams one attachment's bytes.
// GET /api/mail/attachment?sub=&hash=&mailbox=&messageId=&index=
func (s *Server) handleMailAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}
	s.serveAttachmentDownload(w, r, mailClient)
}

func (s *Server) serveAttachmentDownload(w http.ResponseWriter, r *http.Request, mailClient imapadapter.Client) {
	mailbox, uid, err := attachmentRequestParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	index, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("index")))
	if err != nil || index < 0 {
		http.Error(w, "valid index is required", http.StatusBadRequest)
		return
	}
	info, content, err := mailClient.GetAttachment(r.Context(), mailbox, uid, index)
	if errors.Is(err, imapadapter.ErrAttachmentNotFound) {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("attachment fetch failed", "mailbox", mailbox, "uid", strconv.Itoa(uid), "error", err.Error())
		http.Error(w, "failed to fetch attachment", http.StatusBadGateway)
		return
	}

	contentType := strings.TrimSpace(info.MimeType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	name := mailmsg.SanitizeHeaderValue(info.Name)
	if name == "" {
		name = "attachment"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(
		"attachment", map[string]string{"filename": name},
	))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
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

func writeEncryptedPayload(path, keyPath string, payload []byte) error {
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return err
	}

	env, err := cryptutil.Seal(payload, key)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}

	return fsutil.AtomicWriteFile(path, b, 0o600)
}

func decryptEncryptedPayload(raw []byte, keyPath string) ([]byte, error) {
	env, ok := cryptutil.ParseEnvelope(raw)
	if !ok {
		// Backward-compatibility with plaintext credentials.
		return raw, nil
	}

	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	return cryptutil.Open(env, key)
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
		outcome, err := processor.SendWebPush(store, s.cfg.Notifications.PublicKey, s.cfg.Notifications.PrivateKeyPath, 3600, payloadBytes)
		if err != nil {
			http.Error(w, "failed to load notification private key", http.StatusInternalServerError)
			return
		}
		sent = outcome.Sent
		failed = outcome.Failed
		removed = outcome.Removed
	}

	nativeDevices := store.ListNativeDevices()
	nativeSent := 0
	nativeFailed := 0
	nativeRemoved := 0
	nativeError := ""
	if len(nativeDevices) > 0 {
		nativeMessage := processor.NativePushMessage{
			Title: title,
			Body:  body,
			Data:  map[string]string{"url": "/notifications"},
		}
		outcome, err := processor.SendNativePush(r.Context(), s.nativePushDispatcher, s.health, store, nativeMessage, func(device state.NativeDevice, platform string, sendErr error) {
			s.logger.Error("test native notification failed", "device_id", strings.TrimSpace(device.DeviceID), "platform", platform, "sender", "relay", "error", sendErr.Error())
		})
		if outcome.Queued {
			// App Pull mode: queue the test for the device to fetch over HTTP
			// instead of dispatching through the relay/Firebase.
			if err != nil {
				nativeError = "failed to queue pull notification: " + err.Error()
				s.logger.Error("test native pull notification failed", "error", err.Error())
			} else {
				nativeSent = outcome.Sent
			}
		} else {
			nativeSent = outcome.Sent
			nativeFailed = outcome.Failed
			nativeRemoved = outcome.Removed
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
	pullEndpoint := ""
	if serverBaseURL != "" {
		registerEndpoint = strings.TrimRight(serverBaseURL, "/") + "/api/notifications/native/register"
		pullEndpoint = strings.TrimRight(serverBaseURL, "/") + "/api/notifications/native/pull"
	}
	pairingTTLSeconds := int64(90)
	resp := map[string]any{
		"subscriberId":      subscriberID,
		"serverBaseUrl":     serverBaseURL,
		"registerEndpoint":  registerEndpoint,
		"pullEndpoint":      pullEndpoint,
		"deliveryMode":      store.NativeDeliveryMode(),
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
	Transport      string `json:"transport,omitempty"`
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

	platform := normalizeNativePlatform(req.Platform)
	transport, err := normalizeNativeTransport(req.Transport, req.Platform)
	if err != nil {
		http.Error(w, "invalid transport: "+err.Error(), http.StatusBadRequest)
		return
	}

	// For UnifiedPush, the deviceToken is an HTTPS endpoint URL the client
	// fully controls, not an opaque token — reject anything that could be used
	// for SSRF against internal services (private/loopback/link-local hosts).
	// The sender re-checks at send time too, against DNS rebinding.
	if transport == "unifiedpush" {
		if err := processor.ValidateUnifiedPushEndpointURL(deviceToken); err != nil {
			http.Error(w, "invalid unifiedpush deviceToken: "+err.Error(), http.StatusBadRequest)
			return
		}
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
		DeviceID:    strings.TrimSpace(req.DeviceID),
		Platform:    platform,
		Transport:   transport,
		PushToken:   deviceToken,
		DeviceName:  strings.TrimSpace(req.DeviceName),
		AppVersion:  strings.TrimSpace(req.AppVersion),
		UserAgent:   strings.TrimSpace(r.Header.Get("User-Agent")),
		UserID:      ownerID,
		MFAApprover: true,
	}
	if err := store.UpsertNativeDevice(device); err != nil {
		http.Error(w, "failed to persist native device", http.StatusInternalServerError)
		return
	}

	// Resolve the canonical device ID by token: the upsert may have merged
	// this registration into an existing row (same token + platform), whose
	// ID wins over whatever the request carried.
	devices := store.ListNativeDevices()
	registeredDeviceID := device.DeviceID
	for i := len(devices) - 1; i >= 0; i-- {
		if strings.TrimSpace(devices[i].PushToken) == deviceToken && devices[i].Platform == device.Platform {
			registeredDeviceID = devices[i].DeviceID
			break
		}
	}

	serverBaseURL := s.serverBaseURL
	if serverBaseURL == "" {
		serverBaseURL = externalBaseURL(r)
	}
	pullEndpoint := ""
	if serverBaseURL != "" {
		pullEndpoint = strings.TrimRight(serverBaseURL, "/") + "/api/notifications/native/pull"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"synced":       true,
		"deviceId":     registeredDeviceID,
		"devices":      len(devices),
		"deliveryMode": store.NativeDeliveryMode(),
		"pullEndpoint": pullEndpoint,
		"transport":    transport,
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
	if clean == "" {
		// Legacy clients that omit platform entirely default to android.
		return "android"
	}
	// Pass any other platform name through unchanged so a new client isn't
	// silently mislabeled as android — it just shows up under its own name.
	return clean
}

func normalizeNativeTransport(transport, platform string) (string, error) {
	clean := strings.ToLower(strings.TrimSpace(transport))
	switch clean {
	case "fcm", "apns", "unifiedpush":
		return clean, nil
	case "":
		// Derive from platform if transport not specified (legacy behavior).
		switch strings.ToLower(strings.TrimSpace(platform)) {
		case "ios", "macos":
			return "apns", nil
		case "linux":
			return "unifiedpush", nil
		default:
			return "fcm", nil
		}
	default:
		return "", fmt.Errorf("unrecognized transport %q", clean)
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

// handleNotificationNativeMode switches native delivery between the relay-backed
// push mode and App Pull mode for the signed-in user.
func (s *Server) handleNotificationNativeMode(w http.ResponseWriter, r *http.Request) {
	store, err := s.storeFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != state.DeliveryModePush && mode != state.DeliveryModePull {
		http.Error(w, "mode must be \"push\" or \"pull\"", http.StatusBadRequest)
		return
	}
	if err := store.SetNativeDeliveryMode(mode); err != nil {
		http.Error(w, "failed to persist delivery mode", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveryMode": store.NativeDeliveryMode()})
}

// handleNotificationNativePull serves queued notifications to a paired mobile
// app polling over plain HTTP — the App Pull path that bypasses the Cloudflare
// relay and Firebase entirely. It is unauthenticated by web session; the device
// proves ownership with the subscriber id + subscriber hash it received during
// pairing (the same stable HMAC the register endpoint validates). The client
// passes ?after=<cursor> to fetch only notifications newer than its last poll.
func (s *Server) handleNotificationNativePull(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}
	subscriberID := strings.TrimSpace(r.URL.Query().Get("sub"))
	subscriberHash := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hash")))
	if subscriberID == "" || subscriberHash == "" {
		http.Error(w, "sub and hash are required", http.StatusBadRequest)
		return
	}
	expectedHash := s.pairingSubscriberHash(subscriberID)
	if subtle.ConstantTimeCompare([]byte(subscriberHash), []byte(expectedHash)) != 1 {
		http.Error(w, "invalid subscriber hash", http.StatusUnauthorized)
		return
	}
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

	var after int64
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			after = parsed
		}
	}
	notifications, cursor := store.PullNotificationsAfter(after)
	if notifications == nil {
		notifications = []state.PullNotification{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deliveryMode":  store.NativeDeliveryMode(),
		"cursor":        cursor,
		"notifications": notifications,
	})
}

func (s *Server) handleDesktopPair(w http.ResponseWriter, r *http.Request) {
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

	// Check rate limit: max 5 failed attempts per hour
	allowed, remaining, err := store.CheckDesktopPairingRateLimit()
	if err != nil {
		s.logger.Error("rate limit check failed", "user_id", ac.UserID, "error", err.Error())
		http.Error(w, "failed to check rate limit", http.StatusInternalServerError)
		return
	}
	if !allowed {
		s.logger.Error("desktop pairing rate limit exceeded", "user_id", ac.UserID)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate limit exceeded: too many pairing attempts. Try again later.",
		})
		return
	}

	// Generate 16 bytes (128 bits) of cryptographically secure random data
	codeBytes := make([]byte, 16)
	if _, err := rand.Read(codeBytes); err != nil {
		http.Error(w, "failed to generate pairing code", http.StatusInternalServerError)
		return
	}

	// Return as 32-character hex string (no formatting, delivered via API/QR only)
	pairingCode := strings.ToUpper(hex.EncodeToString(codeBytes))

	// Store pairing code with 5-minute expiration
	if err := store.SetDesktopPairingCode(pairingCode, 5*time.Minute); err != nil {
		s.logger.Error("failed to store desktop pairing code", "user_id", ac.UserID, "error", err.Error())
		http.Error(w, "failed to create pairing code", http.StatusInternalServerError)
		return
	}

	// Record successful pairing initiation
	_ = store.RecordDesktopPairingAttempt(pairingCode, true)

	// Log pairing event without exposing the full code (only hash for correlation)
	s.logger.Info("desktop pairing initiated", "user_id", ac.UserID, "code_hash", pairingCode[:8])

	// Build server URL and register endpoint for desktop app
	serverBaseURL := s.serverBaseURL
	if serverBaseURL == "" {
		serverBaseURL = externalBaseURL(r)
	}
	registerEndpoint := ""
	if serverBaseURL != "" {
		registerEndpoint = strings.TrimRight(serverBaseURL, "/") + "/api/notifications/desktop/register"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"pairingCode":      pairingCode,
		"ttlSeconds":       300,
		"rateLimit":        remaining,
		"serverBaseUrl":    serverBaseURL,
		"registerEndpoint": registerEndpoint,
	})
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

// parsePairingTokenUserID decodes and HMAC-verifies token (in the same
// shape as createPairingToken/validatePairingToken) without requiring the
// caller to already know the expected subject, returning the subject the
// token was minted for. Used by the QR key-fetch endpoint, which must learn
// which user a token belongs to rather than confirm a known one — unlike
// validatePairingToken (used for pickup links, where the URL path already
// carries the expected ID to check against).
func (s *Server) parsePairingTokenUserID(token string, now time.Time) (string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return "", errors.New("invalid token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("invalid token payload")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid token signature")
	}

	mac := hmac.New(sha256.New, []byte(s.pairingSecret))
	mac.Write(payload)
	expectedSig := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expectedSig) != 1 {
		return "", errors.New("signature mismatch")
	}

	var claims pairingTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("invalid token claims")
	}
	if claims.Exp <= 0 || now.UTC().Unix() > claims.Exp {
		return "", errors.New("token expired")
	}
	return claims.Sub, nil
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
	// HasAttachments is a warm-path hint for the inbox paperclip badge; see
	// mailcache.Entry.HasAttachments. Absent when false.
	HasAttachments bool `json:"hasAttachments,omitempty"`
	// PGPEncrypted/PGPSigned/PGPVerified/PGPSignerFingerprint/
	// PGPDecryptError mirror imapadapter.MessageContent's PGP fields once
	// decryptPGPMessageContent/decryptPGPUnreadMessage has run.
	PGPEncrypted         bool   `json:"pgpEncrypted,omitempty"`
	PGPSigned            bool   `json:"pgpSigned,omitempty"`
	PGPVerified          bool   `json:"pgpVerified,omitempty"`
	PGPSignerFingerprint string `json:"pgpSignerFingerprint,omitempty"`
	PGPDecryptError      string `json:"pgpDecryptError,omitempty"`
	// ChangeType is only ever set on a delta (since=) response: "new" (Body
	// populated, client should insert) or "updated" (flags/label changed,
	// Body intentionally empty — the client already has it cached). Absent
	// entirely on classic responses, so old clients see no shape change.
	ChangeType string `json:"changeType,omitempty"`
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

// inboxCacheMailboxKey normalizes the mailbox query param into a stable
// mailcache window key: empty (account default) is aliased to "INBOX" so
// omitting the param and passing it explicitly share one window — both
// already resolve to the same selected IMAP folder. The raw (possibly
// empty) mailbox string is still passed to mailClient calls unchanged; this
// normalization is cache-key-only.
func inboxCacheMailboxKey(mailbox string) string {
	trimmed := strings.TrimSpace(mailbox)
	if trimmed == "" || strings.EqualFold(trimmed, "INBOX") {
		return "INBOX"
	}
	return trimmed
}

func mailCacheEntryFromOverview(ov imapadapter.Overview) mailcache.Overview {
	return mailcache.Overview{
		UID:      ov.UID,
		Subject:  ov.Subject,
		Sender:   ov.Sender,
		SentTo:   ov.SentTo,
		CC:       ov.CC,
		BCC:      ov.BCC,
		Keywords: ov.Keywords,
		Status:   ov.Status,
		AtUTC:    ov.AtUTC,
	}
}

func mailCacheEntryFromUnreadMessage(msg imapadapter.UnreadMessage, status string) mailcache.Entry {
	uid, _ := strconv.Atoi(strings.TrimSpace(msg.MessageID))
	return mailcache.Entry{
		UID:                  uid,
		MessageID:            msg.MessageID,
		Subject:              msg.Subject,
		Sender:               msg.Sender,
		SentTo:               msg.SentTo,
		CC:                   msg.CC,
		BCC:                  msg.BCC,
		Keywords:             msg.Keywords,
		Status:               status,
		AtUTC:                msg.AtUTC,
		Body:                 msg.Body,
		HasAttachments:       msg.HasAttachments,
		PGPEncrypted:         msg.PGPEncrypted,
		PGPSigned:            msg.PGPSigned,
		PGPVerified:          msg.PGPVerified,
		PGPSignerFingerprint: msg.PGPSignerFingerprint,
	}
}

// inboxUncategorizedTab is the fallback tab for messages matching none of
// the configured label keywords.
const inboxUncategorizedTab = "Uncategorized"

// buildInboxTabScaffold seeds the tabs/byTab response shape from the
// account's configured label keywords, before any messages are bucketed in
// — shared by handleInbox's no-mail-client empty scaffold and serveInbox's
// populated response, so both start from identical tab ordering.
func buildInboxTabScaffold(allowedKeywords []string) ([]string, map[string][]inboxEmail) {
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

	byTab[inboxUncategorizedTab] = []inboxEmail{}
	return tabs, byTab
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 5000 {
			limit = v
		}
	}
	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))
	useDelta := strings.TrimSpace(r.URL.Query().Get("since")) != ""
	since := parseNonNegativeInt64Query(r, "since")

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	mailClient, err := s.mailFor(r)
	if err != nil {
		// No mailbox configured yet — show the empty tab scaffold rather
		// than an error so the page still renders.
		tabs, byTab := buildInboxTabScaffold(collectAllowedKeywords(cfg))
		tabs = append(tabs, inboxUncategorizedTab)
		writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
		return
	}

	cache, err := s.mailCacheFor(r)
	if err != nil {
		http.Error(w, "failed to open mail cache", http.StatusInternalServerError)
		return
	}

	s.serveInbox(w, r.Context(), ac.UserID, mailClient, cache, cfg, mailbox, limit, since, useDelta)
}

// serveInbox contains handleInbox's core logic once a mail client and cache
// store are resolved — split out from handleInbox (which only does
// param/auth/store resolution) so it can be exercised directly in tests
// against a fake imapadapter.Client, without a real IMAP connection.
func (s *Server) serveInbox(w http.ResponseWriter, ctx context.Context, userID string, mailClient imapadapter.Client, cache *mailcache.Store, cfg config.Config, mailbox string, limit int, since int64, useDelta bool) {
	allowedKeywords := collectAllowedKeywords(cfg)
	tabs, byTab := buildInboxTabScaffold(allowedKeywords)

	// bucket appends entry into the tab its keywords match (or
	// Uncategorized), stamping Label and registering any newly-seen tab —
	// shared by every path below (cache-warmed classic, live-fallback
	// classic, and delta) so bucketing stays identical regardless of where
	// the data came from.
	bucket := func(keywords []string, entry inboxEmail) {
		tab := firstMatchingKeyword(keywords, allowedKeywords)
		if tab == "" {
			tab = inboxUncategorizedTab
		}
		if _, ok := byTab[tab]; !ok {
			byTab[tab] = []inboxEmail{}
			if tab != inboxUncategorizedTab {
				tabs = append(tabs, tab)
			}
		}
		entry.Label = tab
		byTab[tab] = append(byTab[tab], entry)
	}

	cacheKey := inboxCacheMailboxKey(mailbox)

	if !useDelta {
		// Cache-first: if the background poller (or an earlier request)
		// has already warmed a full window of `limit` messages with
		// bodies, serve it with zero IMAP calls.
		if entries, warmed := cache.Snapshot(cacheKey, limit); warmed {
			for _, e := range entries {
				bucket(e.Keywords, inboxEmail{
					MessageID:            e.MessageID,
					Sender:               e.Sender,
					SentTo:               e.SentTo,
					CC:                   e.CC,
					BCC:                  e.BCC,
					Subject:              e.Subject,
					Body:                 e.Body,
					Status:               e.Status,
					AtUTC:                e.AtUTC,
					HasAttachments:       e.HasAttachments,
					PGPEncrypted:         e.PGPEncrypted,
					PGPSigned:            e.PGPSigned,
					PGPVerified:          e.PGPVerified,
					PGPSignerFingerprint: e.PGPSignerFingerprint,
				})
			}
			tabs = append(tabs, inboxUncategorizedTab)
			writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
			return
		}

		// Cold or partial cache (new user, non-INBOX folder the poller
		// never touches, or fewer entries than requested) — fall back to a
		// live fetch exactly as before, then self-warm the cache so the
		// next load for this user+mailbox+limit can be served from it.
		unread, err := mailClient.ListUnreadMessages(ctx, mailbox, limit)
		if err != nil {
			http.Error(w, "failed to fetch inbox", http.StatusBadGateway)
			return
		}

		for i, msg := range unread {
			if msg.PGPEncryptedPayload != "" {
				unread[i] = s.decryptPGPUnreadMessage(userID, msg)
			}
		}

		warmEntries := make([]mailcache.Entry, 0, len(unread))
		for _, msg := range unread {
			status := strings.TrimSpace(msg.Status)
			if status == "" {
				status = "unread"
			}
			bucket(msg.Keywords, inboxEmail{
				MessageID:            msg.MessageID,
				Sender:               msg.Sender,
				SentTo:               msg.SentTo,
				CC:                   msg.CC,
				BCC:                  msg.BCC,
				Subject:              msg.Subject,
				Body:                 msg.Body,
				Status:               status,
				AtUTC:                msg.AtUTC,
				HasAttachments:       msg.HasAttachments,
				PGPEncrypted:         msg.PGPEncrypted,
				PGPSigned:            msg.PGPSigned,
				PGPVerified:          msg.PGPVerified,
				PGPSignerFingerprint: msg.PGPSignerFingerprint,
				PGPDecryptError:      msg.PGPDecryptError,
			})
			warmEntries = append(warmEntries, mailCacheEntryFromUnreadMessage(msg, status))
		}
		if len(warmEntries) > 0 {
			if err := cache.Upsert(cacheKey, warmEntries); err != nil {
				s.logger.Error("failed to warm mail cache", "error", err.Error())
			}
		}

		tabs = append(tabs, inboxUncategorizedTab)
		writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs, "byTab": byTab})
		return
	}

	// Delta path: cheap overview fetch (no bodies), diff against the cache,
	// and only pay for a body fetch on genuinely new messages the cache
	// (and the daemon's opportunistic warming) hasn't already seen.
	overviews, err := mailClient.ListOverviews(ctx, mailbox, limit)
	if err != nil {
		http.Error(w, "failed to fetch inbox", http.StatusBadGateway)
		return
	}
	live := make([]mailcache.Overview, 0, len(overviews))
	for _, ov := range overviews {
		live = append(live, mailCacheEntryFromOverview(ov))
	}

	result, err := cache.Sync(cacheKey, limit, live, since)
	if err != nil {
		http.Error(w, "failed to sync mail cache", http.StatusInternalServerError)
		return
	}

	needBodies := make([]int, 0, len(result.New))
	for _, e := range result.New {
		if e.Body == "" {
			needBodies = append(needBodies, e.UID)
		}
	}
	contents := map[int]imapadapter.MessageContent{}
	if len(needBodies) > 0 {
		contents, err = mailClient.GetMessageBodies(ctx, mailbox, needBodies)
		if err != nil {
			http.Error(w, "failed to fetch inbox", http.StatusBadGateway)
			return
		}
		for uid, c := range contents {
			if c.PGPEncryptedPayload != "" {
				contents[uid] = s.decryptPGPMessageContent(userID, c)
			}
		}
		// Attach the freshly fetched bodies back onto the cache (metadata
		// is unchanged from what Sync just stored, so this only warms
		// Body/HasAttachments without bumping Rev) so a subsequent
		// classic-path load doesn't re-fetch them live.
		warmEntries := make([]mailcache.Entry, 0, len(needBodies))
		for i, e := range result.New {
			if c, ok := contents[e.UID]; ok && c.Body != "" {
				e.Body = c.Body
				e.HasAttachments = c.HasAttachments
				e.PGPEncrypted = c.PGPEncrypted
				e.PGPSigned = c.PGPSigned
				e.PGPVerified = c.PGPVerified
				e.PGPSignerFingerprint = c.PGPSignerFingerprint
				result.New[i] = e
				warmEntries = append(warmEntries, e)
			}
		}
		if len(warmEntries) > 0 {
			if err := cache.Upsert(cacheKey, warmEntries); err != nil {
				s.logger.Error("failed to warm mail cache from delta fetch", "error", err.Error())
			}
		}
	}

	for _, e := range result.New {
		body := e.Body
		hasAttachments := e.HasAttachments
		pgpEncrypted, pgpSigned, pgpVerified := e.PGPEncrypted, e.PGPSigned, e.PGPVerified
		pgpSignerFingerprint := e.PGPSignerFingerprint
		var pgpDecryptError string
		if body == "" {
			if c, ok := contents[e.UID]; ok {
				body = c.Body
				hasAttachments = c.HasAttachments
				pgpEncrypted = c.PGPEncrypted
				pgpSigned = c.PGPSigned
				pgpVerified = c.PGPVerified
				pgpSignerFingerprint = c.PGPSignerFingerprint
				pgpDecryptError = c.PGPDecryptError
			}
		}
		bucket(e.Keywords, inboxEmail{
			MessageID:            e.MessageID,
			Sender:               e.Sender,
			SentTo:               e.SentTo,
			CC:                   e.CC,
			BCC:                  e.BCC,
			Subject:              e.Subject,
			Body:                 body,
			Status:               e.Status,
			AtUTC:                e.AtUTC,
			HasAttachments:       hasAttachments,
			PGPEncrypted:         pgpEncrypted,
			PGPSigned:            pgpSigned,
			PGPVerified:          pgpVerified,
			PGPSignerFingerprint: pgpSignerFingerprint,
			PGPDecryptError:      pgpDecryptError,
			ChangeType:           "new",
		})
	}
	for _, e := range result.Updated {
		bucket(e.Keywords, inboxEmail{
			MessageID:            e.MessageID,
			Sender:               e.Sender,
			SentTo:               e.SentTo,
			CC:                   e.CC,
			BCC:                  e.BCC,
			Subject:              e.Subject,
			Status:               e.Status,
			AtUTC:                e.AtUTC,
			HasAttachments:       e.HasAttachments,
			PGPEncrypted:         e.PGPEncrypted,
			PGPSigned:            e.PGPSigned,
			PGPVerified:          e.PGPVerified,
			PGPSignerFingerprint: e.PGPSignerFingerprint,
			ChangeType:           "updated",
		})
	}

	removed := make([]string, 0, len(result.Removed))
	for _, e := range result.Removed {
		removed = append(removed, e.MessageID)
	}

	tabs = append(tabs, inboxUncategorizedTab)
	writeJSON(w, http.StatusOK, map[string]any{
		"tabs":    tabs,
		"byTab":   byTab,
		"delta":   true,
		"cursor":  result.Cursor,
		"removed": removed,
	})
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

func (s *Server) handleMailSearch(w http.ResponseWriter, r *http.Request) {
	mailClient, err := s.mailFor(r)
	if err != nil {
		if errors.Is(err, errIMAPNotConfigured) {
			http.Error(w, "imap configuration is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		http.Error(w, "q parameter is required", http.StatusBadRequest)
		return
	}

	field := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("field")))
	if field == "" {
		field = "all"
	}
	if field != "subject" && field != "sender" && field != "from" && field != "body" && field != "all" {
		http.Error(w, "invalid field parameter", http.StatusBadRequest)
		return
	}

	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))
	if mailbox == "" {
		mailbox = "INBOX"
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 200 {
		limit = 200
	}

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	allowedKeywords := collectAllowedKeywords(cfg)

	results, err := mailClient.SearchMessages(r.Context(), mailbox, field, q, limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("search failed: %v", err), http.StatusServiceUnavailable)
		return
	}

	// Convert Overview to inboxEmail wire format, mirroring handleInbox's label-bucketing
	out := make([]any, 0, len(results))
	for _, overview := range results {
		label := firstMatchingKeyword(overview.Keywords, allowedKeywords)
		if label == "" {
			label = inboxUncategorizedTab
		}
		out = append(out, inboxEmail{
			MessageID:      overview.MessageID,
			Subject:        overview.Subject,
			Sender:         overview.Sender,
			SentTo:         overview.SentTo,
			CC:             overview.CC,
			BCC:            overview.BCC,
			Label:          label,
			Status:         overview.Status,
			AtUTC:          overview.AtUTC,
			HasAttachments: false,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"results": out,
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
	logDir := config.EnvOrDefault("LOG_DIR", "/llama_lab/logs")
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
	logDir := config.EnvOrDefault("LOG_DIR", "/llama_lab/logs")
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
	admin := users.FirstAdminFrom(all)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "setup": map[string]any{"admin_user": admin.Username, "must_change_password": admin.MustChangePassword}})
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

	// Second-factor users must clear a challenge before a session exists. No
	// cookie is set here; the client receives a challenge id plus the methods it
	// may use. A push-enabled challenge additionally fans a notification out to
	// the user's approver devices (asynchronously — see dispatchPushChallenge).
	if u.TOTPEnabled || u.PushMFAEnabled {
		ch, err := s.mfaChallenges.Create(u.ID)
		if err != nil {
			http.Error(w, "session creation failed", http.StatusInternalServerError)
			return
		}
		methods := make([]string, 0, 2)
		if u.TOTPEnabled {
			methods = append(methods, "totp")
		}
		if u.PushMFAEnabled {
			methods = append(methods, "push")
			go s.dispatchPushChallenge(u.ID, ch.ID)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mfaRequired": true,
			"challengeId": ch.ID,
			"methods":     methods,
		})
		return
	}

	if err := s.startSession(w, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

// startSession mints a session token for userID, records it, and sets the
// llama_session cookie with exactly the flags the legacy password-only login
// used. Shared by handleLogin and the second-factor endpoints.
func (s *Server) startSession(w http.ResponseWriter, userID string) error {
	token, err := randomToken(24)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sessions[token] = Session{UserID: userID, ExpiresAt: time.Now().Add(24 * time.Hour)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "llama_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return nil
}

// handleMFATOTP completes a login challenge with a TOTP code. It is
// authenticated solely by possession of a valid challengeId (no session
// cookie). On success it mints the real session.
func (s *Server) handleMFATOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ch, ok := s.mfaChallenges.Get(strings.TrimSpace(req.ChallengeID))
	if !ok {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	u, err := s.users.Get(ch.UserID)
	if err != nil || !u.Active || !u.TOTPEnabled || u.TOTPSecretEnc == "" {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	secret, err := mfa.OpenTOTPSecret(u.TOTPSecretEnc, s.totpSecretKeyPath)
	if err != nil {
		http.Error(w, "failed to load second factor", http.StatusInternalServerError)
		return
	}

	step, valid := totp.Validate(secret, req.Code, time.Now())
	if !valid {
		if err := s.mfaChallenges.RecordTOTPAttempt(ch.ID); errors.Is(err, mfa.ErrTooManyAttempts) {
			http.Error(w, "too many attempts", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}

	// A challenge is single-use: ConsumeTOTPStep atomically checks-and-marks
	// consumption under a single lock, so two concurrent requests bearing the
	// same still-valid code cannot both win (closes the TOCTOU window a
	// separate Get + later RecordTOTPStep would leave open).
	if err := s.mfaChallenges.ConsumeTOTPStep(ch.ID, step); err != nil {
		if errors.Is(err, mfa.ErrChallengeAlreadyUsed) {
			http.Error(w, "challenge already used", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	s.mfaChallenges.Delete(ch.ID)
	if err := s.startSession(w, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

// handleMFARecoveryCode completes a login challenge with a one-time recovery
// code. The matched code is consumed (removed) on success.
func (s *Server) handleMFARecoveryCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ch, ok := s.mfaChallenges.Get(strings.TrimSpace(req.ChallengeID))
	if !ok {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	u, err := s.users.Get(ch.UserID)
	if err != nil || !u.Active || !u.TOTPEnabled {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	_, matched, err := s.users.ConsumeRecoveryCode(u.ID, strings.TrimSpace(req.Code))
	if err != nil {
		http.Error(w, "failed to verify recovery code", http.StatusInternalServerError)
		return
	}
	if !matched {
		if err := s.mfaChallenges.RecordTOTPAttempt(ch.ID); errors.Is(err, mfa.ErrTooManyAttempts) {
			http.Error(w, "too many attempts", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}

	s.mfaChallenges.Delete(ch.ID)
	if err := s.startSession(w, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
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
	frontendDir := config.EnvOrDefault("FRONTEND_DIR", "/opt/llama-lab/frontend")
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

// withMailAuth gates endpoints mobile clients need to reach without a web
// session — mail read/act-on (inbox, folders, actions, draft, send), contacts
// dedupe/groups/photo-get, and the PGP QR token mint — for either a web
// session cookie or mobile's paired subscriberId/subscriberHash — see
// resolveMailAuthContext. Despite the name, it's no longer mail-exclusive;
// IMAP/SMTP account setup (/api/imap/config, /api/imap/test) and other
// web-UI-only writes intentionally stay on withAuth only.
func (s *Server) withMailAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, err := s.resolveMailAuthContext(r)
		if err != nil {
			if errors.Is(err, errMailPairingNotConfigured) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, ac)))
	}
}

type authContextKey struct{}

// authFromContext retrieves the AuthContext injected by withAuth or
// withDAVBasicAuth. It only returns ok=false if called on a request that
// never passed through either (a programming error), since both already
// reject the request before next() runs otherwise.
func authFromContext(r *http.Request) (AuthContext, bool) {
	return authContextFromContext(r.Context())
}

func authContextFromContext(ctx context.Context) (AuthContext, bool) {
	ac, ok := ctx.Value(authContextKey{}).(AuthContext)
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
