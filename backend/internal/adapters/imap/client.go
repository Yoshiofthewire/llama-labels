package imap

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	goimap "github.com/BrianLeishman/go-imap"
)

type Message struct {
	ID      string
	Subject string
	Sender  string
	SentTo  string
	Body    string
}

type UnreadMessage struct {
	MessageID string
	Subject   string
	Sender    string
	SentTo    string
	Keywords  []string
	AtUTC     string
	Body      string
	Status    string
}

var htmlTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

func htmlToPlainText(htmlBody string) string {
	trimmed := strings.TrimSpace(htmlBody)
	if trimmed == "" {
		return ""
	}
	noTags := htmlTagPattern.ReplaceAllString(trimmed, " ")
	unescaped := stdhtml.UnescapeString(noTags)
	lines := strings.Split(unescaped, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		normalized := strings.Join(strings.Fields(line), " ")
		if normalized == "" {
			continue
		}
		cleaned = append(cleaned, normalized)
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

type Client interface {
	ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error)
	ListUnreadMessages(ctx context.Context, limit int) ([]UnreadMessage, error)
	ListLabels(ctx context.Context) ([]string, error)
	ListSubfolders(ctx context.Context, parent string) ([]string, error)
	EnsureLabel(ctx context.Context, label string) error
	ApplyLabel(ctx context.Context, messageID, label string) error
	ApplyInboxAction(ctx context.Context, messageID, action string) error
}

type StubClient struct{}

func (s *StubClient) ListUnreadInbox(_ context.Context, _ string) ([]Message, string, error) {
	return []Message{}, "", nil
}

func (s *StubClient) ListUnreadMessages(_ context.Context, _ int) ([]UnreadMessage, error) {
	return []UnreadMessage{}, nil
}

func (s *StubClient) ListLabels(_ context.Context) ([]string, error) {
	return []string{}, nil
}

func (s *StubClient) ListSubfolders(_ context.Context, _ string) ([]string, error) {
	return []string{}, nil
}

func (s *StubClient) EnsureLabel(_ context.Context, _ string) error {
	return nil
}

func (s *StubClient) ApplyLabel(_ context.Context, _ string, _ string) error {
	return nil
}

func (s *StubClient) ApplyInboxAction(_ context.Context, _ string, _ string) error {
	return nil
}

type APIClient struct {
	mu       sync.Mutex
	dialer   *goimap.Dialer
	host     string
	port     int
	username string
	password string
	mailbox  string
}

type storedIMAPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Mailbox  string `json:"mailbox"`
}

type encryptedPayload struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func NewAPIClientFromEnv() *APIClient {
	host := strings.TrimSpace(os.Getenv("IMAP_HOST"))
	username := strings.TrimSpace(os.Getenv("IMAP_USERNAME"))
	password := strings.TrimSpace(os.Getenv("IMAP_PASSWORD"))
	mailbox := strings.TrimSpace(os.Getenv("IMAP_MAILBOX"))
	if mailbox == "" {
		mailbox = "INBOX"
	}

	port := 993
	if raw := strings.TrimSpace(os.Getenv("IMAP_PORT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			port = parsed
		}
	}

	return &APIClient{
		host:     host,
		port:     port,
		username: username,
		password: password,
		mailbox:  mailbox,
	}
}

func defaultConfigPath() string {
	path := strings.TrimSpace(os.Getenv("IMAP_CONFIG_FILE"))
	if path == "" {
		path = "/llama_lab/private/imap-config.json"
	}
	return path
}

func defaultConfigKeyPath() string {
	path := strings.TrimSpace(os.Getenv("IMAP_CONFIG_KEY_FILE"))
	if path == "" {
		path = "/llama_lab/private/imap-config.key"
	}
	return path
}

func (c *APIClient) ensureCredentialsFromStoredConfigLocked() error {
	if strings.TrimSpace(c.host) != "" && strings.TrimSpace(c.username) != "" && strings.TrimSpace(c.password) != "" {
		return nil
	}

	raw, err := os.ReadFile(defaultConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read imap config: %w", err)
	}

	plain, err := decryptStoredPayload(raw, defaultConfigKeyPath())
	if err != nil {
		return fmt.Errorf("decrypt imap config: %w", err)
	}

	var payload storedIMAPConfig
	if err := json.Unmarshal(plain, &payload); err != nil {
		return fmt.Errorf("parse imap config: %w", err)
	}

	payload.Host = strings.TrimSpace(payload.Host)
	payload.Username = strings.TrimSpace(payload.Username)
	payload.Password = strings.TrimSpace(payload.Password)
	payload.Mailbox = strings.TrimSpace(payload.Mailbox)
	if payload.Port <= 0 {
		payload.Port = 993
	}
	if payload.Mailbox == "" {
		payload.Mailbox = "INBOX"
	}

	if payload.Host == "" || payload.Username == "" || payload.Password == "" {
		return nil
	}

	c.host = payload.Host
	c.port = payload.Port
	c.username = payload.Username
	c.password = payload.Password
	if strings.TrimSpace(c.mailbox) == "" || c.mailbox == "INBOX" {
		c.mailbox = payload.Mailbox
	}

	return nil
}

func decryptStoredPayload(raw []byte, keyPath string) ([]byte, error) {
	var env encryptedPayload
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != 1 || strings.TrimSpace(env.Nonce) == "" || strings.TrimSpace(env.Ciphertext) == "" {
		// Backward-compatibility with plaintext credentials.
		return raw, nil
	}

	keyRaw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyRaw)))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid encryption master key length")
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

func (c *APIClient) ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, "", err
	}

	uids, err := d.GetUIDs("UNSEEN")
	if err != nil {
		return nil, "", fmt.Errorf("imap search unseen: %w", err)
	}
	if len(uids) == 0 {
		return []Message{}, sinceCheckpoint, nil
	}

	minUID := parseCheckpointUID(sinceCheckpoint)
	filtered := make([]int, 0, len(uids))
	for _, uid := range uids {
		if uid > minUID {
			filtered = append(filtered, uid)
		}
	}
	if len(filtered) == 0 {
		return []Message{}, sinceCheckpoint, nil
	}
	sort.Ints(filtered)

	emails, err := d.GetEmails(filtered...)
	if err != nil {
		return nil, "", fmt.Errorf("imap fetch emails: %w", err)
	}

	out := make([]Message, 0, len(filtered))
	maxUID := minUID
	for _, uid := range filtered {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		e := emails[uid]
		if e == nil {
			continue
		}
		body := strings.TrimSpace(e.Text)
		if body == "" {
			body = strings.TrimSpace(e.HTML)
		}
		out = append(out, Message{
			ID:      strconv.Itoa(uid),
			Subject: strings.TrimSpace(e.Subject),
			Sender:  strings.TrimSpace(e.From.String()),
			SentTo:  strings.TrimSpace(e.To.String()),
			Body:    body,
		})
		if uid > maxUID {
			maxUID = uid
		}
	}

	next := sinceCheckpoint
	if maxUID > minUID {
		next = strconv.Itoa(maxUID)
	}
	return out, next, nil
}

func (c *APIClient) ListUnreadMessages(ctx context.Context, limit int) ([]UnreadMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 500
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}

	uids, err := d.GetLastNUIDs(limit)
	if err != nil {
		return nil, fmt.Errorf("imap list recent messages: %w", err)
	}
	if len(uids) == 0 {
		return []UnreadMessage{}, nil
	}

	sort.Ints(uids)

	emails, err := d.GetEmails(uids...)
	if err != nil {
		return nil, fmt.Errorf("imap fetch emails: %w", err)
	}

	overviews, err := d.GetOverviews(uids...)
	if err != nil {
		return nil, fmt.Errorf("imap fetch overviews: %w", err)
	}

	out := make([]UnreadMessage, 0, len(uids))
	for i := len(uids) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		uid := uids[i]
		e := emails[uid]
		if e == nil {
			continue
		}

		keywords := []string{}
		status := "unread"
		if ov := overviews[uid]; ov != nil {
			seen := map[string]bool{}
			for _, flag := range ov.Flags {
				clean := strings.TrimSpace(flag)
				if clean == "" {
					continue
				}
				if strings.EqualFold(clean, "\\Seen") {
					status = "read"
					continue
				}
				if strings.HasPrefix(clean, "\\") {
					continue
				}
				key := strings.ToLower(clean)
				if seen[key] {
					continue
				}
				seen[key] = true
				keywords = append(keywords, clean)
			}
		}

		ts := e.Sent
		if ts.IsZero() {
			ts = e.Received
		}
		atUTC := ""
		if !ts.IsZero() {
			atUTC = ts.UTC().Format(time.RFC3339)
		}

		// Prefer HTML for inbox preview so the UI can render rich email content.
		// Fall back to plain text for text-only messages.
		body := strings.TrimSpace(e.HTML)
		if body == "" {
			body = strings.TrimSpace(e.Text)
		}

		out = append(out, UnreadMessage{
			MessageID: strconv.Itoa(uid),
			Subject:   strings.TrimSpace(e.Subject),
			Sender:    strings.TrimSpace(e.From.String()),
			SentTo:    strings.TrimSpace(e.To.String()),
			Keywords:  keywords,
			AtUTC:     atUTC,
			Body:      body,
			Status:    status,
		})
	}

	return out, nil
}

func (c *APIClient) ListLabels(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}

	lastUIDs, err := d.GetLastNUIDs(200)
	if err != nil {
		return nil, fmt.Errorf("imap get recent uids: %w", err)
	}
	if len(lastUIDs) == 0 {
		return []string{}, nil
	}

	ov, err := d.GetOverviews(lastUIDs...)
	if err != nil {
		return nil, fmt.Errorf("imap get overviews: %w", err)
	}

	seen := map[string]bool{}
	labels := make([]string, 0, 16)
	for _, uid := range lastUIDs {
		o := ov[uid]
		if o == nil {
			continue
		}
		for _, flag := range o.Flags {
			flag = strings.TrimSpace(flag)
			if flag == "" || strings.HasPrefix(flag, "\\") {
				continue
			}
			if seen[flag] {
				continue
			}
			seen[flag] = true
			labels = append(labels, flag)
		}
	}
	sort.Strings(labels)
	return labels, nil
}

func (c *APIClient) ListSubfolders(ctx context.Context, parent string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return nil, errors.New("parent folder is required")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}

	folders, err := d.GetFolders()
	if err != nil {
		return nil, fmt.Errorf("imap list folders: %w", err)
	}

	parentLower := strings.ToLower(parent)
	children := []string{}
	seen := map[string]bool{}
	for _, folder := range folders {
		clean := strings.TrimSpace(folder)
		if clean == "" {
			continue
		}
		child := ""
		for _, prefix := range []string{parent + "/", parent + ".", "INBOX/" + parent + "/", "INBOX." + parent + "."} {
			if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(prefix)) {
				child = clean[len(prefix):]
				break
			}
		}
		if child == "" {
			continue
		}
		if idx := strings.IndexAny(child, "/."); idx >= 0 {
			child = child[:idx]
		}
		child = strings.TrimSpace(child)
		if child == "" {
			continue
		}
		key := strings.ToLower(child)
		if key == parentLower || seen[key] {
			continue
		}
		seen[key] = true
		children = append(children, child)
	}

	sort.Strings(children)
	return children, nil
}

func (c *APIClient) EnsureLabel(ctx context.Context, label string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" {
		return errors.New("label is required")
	}
	// IMAP keywords are typically created implicitly when first applied.
	return nil
}

func (c *APIClient) ApplyLabel(ctx context.Context, messageID, label string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(messageID))
	if err != nil || uid <= 0 {
		return fmt.Errorf("invalid message id %q", messageID)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return errors.New("label is required")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}

	flags := goimap.Flags{Keywords: map[string]bool{label: true}}
	if err := d.SetFlags(uid, flags); err != nil {
		return fmt.Errorf("imap set keyword %q on uid %d: %w", label, uid, err)
	}
	return nil
}

func (c *APIClient) ApplyInboxAction(ctx context.Context, messageID, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(messageID))
	if err != nil || uid <= 0 {
		return fmt.Errorf("invalid message id %q", messageID)
	}
	action = strings.ToLower(strings.TrimSpace(action))

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}

	moveToFolder := func(folder string) error {
		if err := d.MoveEmail(uid, folder); err == nil {
			return nil
		}
		if err := d.CreateFolder(folder); err != nil {
			return err
		}
		return d.MoveEmail(uid, folder)
	}

	switch action {
	case "read":
		if err := d.MarkSeen(uid); err != nil {
			return fmt.Errorf("imap mark seen uid %d: %w", uid, err)
		}
		return nil
	case "archive":
		if err := moveToFolder("Archive"); err != nil {
			return fmt.Errorf("imap move uid %d to Archive: %w", uid, err)
		}
		return nil
	case "spam":
		if err := moveToFolder("Spam"); err != nil {
			return fmt.Errorf("imap move uid %d to Spam: %w", uid, err)
		}
		return nil
	case "delete":
		if err := d.DeleteEmail(uid); err != nil {
			return fmt.Errorf("imap delete uid %d: %w", uid, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported inbox action %q", action)
	}
}

func (c *APIClient) ensureConnectedLocked() (*goimap.Dialer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureCredentialsFromStoredConfigLocked(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(c.host) == "" || strings.TrimSpace(c.username) == "" || strings.TrimSpace(c.password) == "" {
		return nil, errors.New("missing IMAP credentials; configure IMAP_HOST, IMAP_USERNAME, and IMAP_PASSWORD or save credentials in IMAP settings")
	}

	if c.dialer == nil {
		goimap.DialTimeout = 10 * time.Second
		goimap.CommandTimeout = 45 * time.Second
		goimap.RetryCount = 3

		d, err := goimap.New(c.username, c.password, c.host, c.port)
		if err != nil {
			return nil, fmt.Errorf("imap connect: %w", err)
		}
		c.dialer = d
	}

	if err := c.dialer.SelectFolder(c.mailbox); err != nil {
		if recErr := c.dialer.Reconnect(); recErr != nil {
			return nil, fmt.Errorf("imap select folder %q: %w", c.mailbox, err)
		}
		if err := c.dialer.SelectFolder(c.mailbox); err != nil {
			return nil, fmt.Errorf("imap select folder %q after reconnect: %w", c.mailbox, err)
		}
	}

	return c.dialer, nil
}

func parseCheckpointUID(checkpoint string) int {
	v := strings.TrimSpace(checkpoint)
	if v == "" {
		return 0
	}
	uid, err := strconv.Atoi(v)
	if err != nil || uid < 0 {
		return 0
	}
	return uid
}
