package imap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"llama-lab/backend/internal/cryptutil"
	"llama-lab/backend/internal/mailmsg"

	goimap "github.com/BrianLeishman/go-imap"
)

type Message struct {
	ID       string
	Subject  string
	Sender   string
	SentTo   string
	CC       string
	BCC      string
	Keywords []string
	AtUTC    string
	Body     string
	// HasAttachments is set from the same GetEmails parse that fills Body, so
	// the poller's cache-warm path can carry it into mailcache.Entry without
	// any extra IMAP round trip.
	HasAttachments bool
}

type UnreadMessage struct {
	MessageID string
	Subject   string
	Sender    string
	SentTo    string
	CC        string
	BCC       string
	Keywords  []string
	AtUTC     string
	Body      string
	Status    string
	// HasAttachments comes from the same GetEmails parse as Body.
	HasAttachments bool
}

// MessageContent is the per-UID result of GetMessageBodies: the rendered body
// plus whether the message carries attachments, both from one GetEmails parse.
type MessageContent struct {
	Body           string
	HasAttachments bool
}

// Overview is UID + envelope + flags for one message, without body content
// — backed by GetOverviews (UID FETCH ... ALL), which per RFC 3501 never
// includes body text. Used by the mail-cache sync path (ListOverviews) so
// the expensive body fetch (GetMessageBodies) happens only for UIDs the
// cache doesn't already have.
type Overview struct {
	MessageID string
	Subject   string
	Sender    string
	SentTo    string
	CC        string
	BCC       string
	Keywords  []string
	AtUTC     string
	Status    string
	UID       int
}

type DraftMessage struct {
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	Body        string
	Mode        string
	Attachments []mailmsg.Attachment
}

// AttachmentInfo is one attachment's metadata, without its content. JSON
// tags match the /api/mail/attachments wire shape.
type AttachmentInfo struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int    `json:"size"`
}

// ErrAttachmentNotFound reports an attachment index that doesn't exist on
// the message; the API maps it to 404.
var ErrAttachmentNotFound = errors.New("attachment not found")

type Client interface {
	ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error)
	ListUnreadMessages(ctx context.Context, mailbox string, limit int) ([]UnreadMessage, error)
	// ListOverviews returns UID + envelope + flags for the last N messages
	// in mailbox, without a body fetch — the selective, cheap counterpart
	// to ListUnreadMessages used by the mail cache's live-diff path.
	ListOverviews(ctx context.Context, mailbox string, limit int) ([]Overview, error)
	// GetMessageBodies fetches body content and attachment presence for
	// exactly the given UIDs — called only for UIDs the mail cache reports as
	// genuinely new.
	GetMessageBodies(ctx context.Context, mailbox string, uids []int) (map[int]MessageContent, error)
	ListLabels(ctx context.Context) ([]string, error)
	ListSubfolders(ctx context.Context, parent string) ([]string, error)
	CreateFolder(ctx context.Context, parent, name string) (string, error)
	RenameFolder(ctx context.Context, folder, name string) (string, error)
	DeleteFolder(ctx context.Context, folder string) error
	EnsureLabel(ctx context.Context, label string) error
	ApplyLabel(ctx context.Context, messageID, label string) error
	ApplyInboxAction(ctx context.Context, messageID, action, mailbox, targetMailbox string) error
	// ListAttachments returns attachment metadata for one message (UID).
	ListAttachments(ctx context.Context, mailbox string, uid int) ([]AttachmentInfo, error)
	// GetAttachment returns one attachment's metadata and content by index.
	GetAttachment(ctx context.Context, mailbox string, uid int, index int) (AttachmentInfo, []byte, error)
	SaveDraft(ctx context.Context, draft DraftMessage) error
	SaveSent(ctx context.Context, draft DraftMessage) error
}

type APIClient struct {
	mu       sync.Mutex
	opMu     sync.Mutex
	dialer   *goimap.Dialer
	host     string
	port     int
	username string
	password string
	mailbox  string

	// configPath/configKeyPath override the process-wide default stored
	// config location so one client can be built per user's credential file.
	configPath    string
	configKeyPath string
}

type storedIMAPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Mailbox  string `json:"mailbox"`
}

// NewAPIClientFromStoredConfig builds a client that loads its credentials
// from a specific encrypted config file (per-user), never from env vars.
func NewAPIClientFromStoredConfig(configPath, configKeyPath string) *APIClient {
	return &APIClient{
		port:          993,
		mailbox:       "INBOX",
		configPath:    configPath,
		configKeyPath: configKeyPath,
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

	configPath := c.configPath
	if strings.TrimSpace(configPath) == "" {
		configPath = defaultConfigPath()
	}
	keyPath := c.configKeyPath
	if strings.TrimSpace(keyPath) == "" {
		keyPath = defaultConfigKeyPath()
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read imap config: %w", err)
	}

	plain, err := decryptStoredPayload(raw, keyPath)
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
	env, ok := cryptutil.ParseEnvelope(raw)
	if !ok {
		// Backward-compatibility with plaintext credentials.
		return raw, nil
	}

	// imap never creates the master key — only the api process does; a
	// missing key here is an error, not a reason to generate a new one.
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		return nil, err
	}
	return cryptutil.Open(env, key)
}

// overviewFromEmail builds an Overview from a go-imap *Email, parsing IMAP
// flags into Keywords/Status (a \Seen flag maps to Status "read", leading
// backslash flags are otherwise ignored, everything else is a label
// keyword). Works regardless of whether e came from GetOverviews directly
// or from GetEmails (which internally calls GetOverviews first and never
// overwrites Flags/Sent/Received when it later merges in body content).
func overviewFromEmail(uid int, e *goimap.Email) Overview {
	if e == nil {
		return Overview{MessageID: strconv.Itoa(uid), UID: uid, Status: "unread"}
	}

	keywords := []string{}
	status := "unread"
	seen := map[string]bool{}
	for _, flag := range e.Flags {
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

	ts := e.Sent
	if ts.IsZero() {
		ts = e.Received
	}
	atUTC := ""
	if !ts.IsZero() {
		atUTC = ts.UTC().Format(time.RFC3339)
	}

	return Overview{
		MessageID: strconv.Itoa(uid),
		Subject:   strings.TrimSpace(e.Subject),
		Sender:    strings.TrimSpace(e.From.String()),
		SentTo:    strings.TrimSpace(e.To.String()),
		CC:        strings.TrimSpace(e.CC.String()),
		BCC:       strings.TrimSpace(e.BCC.String()),
		Keywords:  keywords,
		AtUTC:     atUTC,
		Status:    status,
		UID:       uid,
	}
}

func (c *APIClient) ListUnreadInbox(ctx context.Context, sinceCheckpoint string) ([]Message, string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

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
		ov := overviewFromEmail(uid, e)
		out = append(out, Message{
			ID:             ov.MessageID,
			Subject:        ov.Subject,
			Sender:         ov.Sender,
			SentTo:         ov.SentTo,
			CC:             ov.CC,
			BCC:            ov.BCC,
			Keywords:       ov.Keywords,
			AtUTC:          ov.AtUTC,
			Body:           body,
			HasAttachments: len(e.Attachments) > 0,
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

func (c *APIClient) ListUnreadMessages(ctx context.Context, mailbox string, limit int) ([]UnreadMessage, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 500
	}
	mailbox = strings.TrimSpace(mailbox)

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if mailbox != "" && !strings.EqualFold(mailbox, c.mailbox) {
		if err := d.SelectFolder(mailbox); err != nil {
			return nil, fmt.Errorf("imap select folder %q: %w", mailbox, err)
		}
	}

	uids, err := d.GetLastNUIDs(limit)
	if err != nil {
		return nil, fmt.Errorf("imap list recent messages: %w", err)
	}
	if len(uids) == 0 {
		return []UnreadMessage{}, nil
	}

	sort.Ints(uids)

	// A single GetEmails call is enough: it internally calls GetOverviews
	// first and never overwrites Flags/Sent/Received when it later merges
	// in body content, so overviewFromEmail(uid, e) below already has
	// everything a second, separate GetOverviews call used to provide.
	emails, err := d.GetEmails(uids...)
	if err != nil {
		return nil, fmt.Errorf("imap fetch emails: %w", err)
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

		ov := overviewFromEmail(uid, e)

		// Prefer HTML for inbox preview so the UI can render rich email content.
		// Fall back to plain text for text-only messages.
		body := strings.TrimSpace(e.HTML)
		if body == "" {
			body = strings.TrimSpace(e.Text)
		}

		out = append(out, UnreadMessage{
			MessageID:      ov.MessageID,
			Subject:        ov.Subject,
			Sender:         ov.Sender,
			SentTo:         ov.SentTo,
			CC:             ov.CC,
			BCC:            ov.BCC,
			Keywords:       ov.Keywords,
			AtUTC:          ov.AtUTC,
			Body:           body,
			Status:         ov.Status,
			HasAttachments: len(e.Attachments) > 0,
		})
	}

	return out, nil
}

// ListOverviews returns UID + envelope + flags for the last N messages in
// mailbox, without a body fetch (GetLastNUIDs + GetOverviews only — no
// GetEmails/body FETCH). Used by the mail-cache Sync path so the expensive
// body fetch happens only for UIDs the cache doesn't already have.
func (c *APIClient) ListOverviews(ctx context.Context, mailbox string, limit int) ([]Overview, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 500
	}
	mailbox = strings.TrimSpace(mailbox)

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if mailbox != "" && !strings.EqualFold(mailbox, c.mailbox) {
		if err := d.SelectFolder(mailbox); err != nil {
			return nil, fmt.Errorf("imap select folder %q: %w", mailbox, err)
		}
	}

	uids, err := d.GetLastNUIDs(limit)
	if err != nil {
		return nil, fmt.Errorf("imap list recent messages: %w", err)
	}
	if len(uids) == 0 {
		return []Overview{}, nil
	}

	sort.Ints(uids)

	overviews, err := d.GetOverviews(uids...)
	if err != nil {
		return nil, fmt.Errorf("imap fetch overviews: %w", err)
	}

	out := make([]Overview, 0, len(uids))
	for i := len(uids) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		uid := uids[i]
		e := overviews[uid]
		if e == nil {
			continue
		}
		out = append(out, overviewFromEmail(uid, e))
	}
	return out, nil
}

// GetMessageBodies fetches full body content (HTML preferred, falling back
// to plain text) and attachment presence for exactly the given UIDs — the
// selective counterpart to ListOverviews, called only for UIDs the mail cache
// reports as new.
func (c *APIClient) GetMessageBodies(ctx context.Context, mailbox string, uids []int) (map[int]MessageContent, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return map[int]MessageContent{}, nil
	}
	mailbox = strings.TrimSpace(mailbox)

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if mailbox != "" && !strings.EqualFold(mailbox, c.mailbox) {
		if err := d.SelectFolder(mailbox); err != nil {
			return nil, fmt.Errorf("imap select folder %q: %w", mailbox, err)
		}
	}

	emails, err := d.GetEmails(uids...)
	if err != nil {
		return nil, fmt.Errorf("imap fetch emails: %w", err)
	}

	out := make(map[int]MessageContent, len(uids))
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := emails[uid]
		if e == nil {
			continue
		}
		body := strings.TrimSpace(e.HTML)
		if body == "" {
			body = strings.TrimSpace(e.Text)
		}
		out[uid] = MessageContent{Body: body, HasAttachments: len(e.Attachments) > 0}
	}
	return out, nil
}

func (c *APIClient) ListLabels(ctx context.Context) ([]string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

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
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent = strings.TrimSpace(parent)

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}

	folders, err := d.GetFolders()
	if err != nil {
		return nil, fmt.Errorf("imap list folders: %w", err)
	}

	if parent == "" {
		children := []string{}
		seen := map[string]bool{}
		for _, folder := range folders {
			clean := strings.TrimSpace(folder)
			if clean == "" || strings.EqualFold(clean, "INBOX") {
				continue
			}

			topLevel := clean
			if strings.HasPrefix(strings.ToUpper(clean), "INBOX/") || strings.HasPrefix(strings.ToUpper(clean), "INBOX.") {
				rest := clean[len("INBOX/"):]
				if strings.HasPrefix(strings.ToUpper(clean), "INBOX.") {
					rest = clean[len("INBOX."):]
				}
				if idx := strings.IndexAny(rest, "/."); idx >= 0 {
					rest = rest[:idx]
				}
				sep := "/"
				if strings.HasPrefix(strings.ToUpper(clean), "INBOX.") {
					sep = "."
				}
				topLevel = "INBOX" + sep + strings.TrimSpace(rest)
			} else if idx := strings.IndexAny(clean, "/."); idx >= 0 {
				topLevel = clean[:idx]
			}

			label := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(topLevel, "INBOX/"), "INBOX."))
			if label == "" || strings.EqualFold(label, "Archive") {
				continue
			}
			key := strings.ToLower(topLevel)
			if seen[key] {
				continue
			}
			seen[key] = true
			children = append(children, topLevel)
		}

		sort.Strings(children)
		return children, nil
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
				rest := clean[len(prefix):]
				if rest == "" {
					break
				}
				child = clean
				if idx := strings.IndexAny(rest, "/."); idx >= 0 {
					child = prefix + rest[:idx]
				}
				break
			}
		}
		if child == "" {
			continue
		}
		label := strings.TrimSpace(child)
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower("INBOX/"+parent+"/")) {
			label = label[len("INBOX/"+parent+"/"):]
		} else if strings.HasPrefix(strings.ToLower(label), strings.ToLower("INBOX."+parent+".")) {
			label = label[len("INBOX."+parent+"."):]
		} else if strings.HasPrefix(strings.ToLower(label), strings.ToLower(parent+"/")) {
			label = label[len(parent+"/"):]
		} else if strings.HasPrefix(strings.ToLower(label), strings.ToLower(parent+".")) {
			label = label[len(parent+"."):]
		}
		label = strings.TrimSpace(label)
		if label == "" {
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

func containsMailboxPath(folders []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, folder := range folders {
		if strings.EqualFold(strings.TrimSpace(folder), target) {
			return true
		}
	}
	return false
}

func preferredMailboxDelimiters(parent string, folders []string) []string {
	clean := strings.TrimSpace(parent)
	if strings.Contains(clean, "/") {
		return []string{"/", "."}
	}
	if strings.Contains(clean, ".") {
		return []string{".", "/"}
	}
	for _, folder := range folders {
		trimmed := strings.TrimSpace(folder)
		if strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(clean+"/")) {
			return []string{"/", "."}
		}
		if strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(clean+".")) {
			return []string{".", "/"}
		}
	}
	if strings.EqualFold(clean, "INBOX") {
		return []string{"/", "."}
	}
	return []string{"/", "."}
}

func mailboxParent(path string) string {
	clean := strings.TrimSpace(path)
	idx := strings.LastIndexAny(clean, "/.")
	if idx <= 0 {
		return ""
	}
	return clean[:idx]
}

func (c *APIClient) CreateFolder(ctx context.Context, parent, name string) (string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", err
	}

	parent = strings.TrimSpace(parent)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("folder name is required")
	}
	if strings.ContainsAny(name, "/.") {
		return "", errors.New("folder name must be a single level without / or .")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return "", err
	}

	folders, err := d.GetFolders()
	if err != nil {
		return "", fmt.Errorf("imap list folders: %w", err)
	}

	if parent == "" {
		if containsMailboxPath(folders, name) {
			return name, nil
		}
		if err := d.CreateFolder(name); err != nil {
			return "", fmt.Errorf("imap create folder %q: %w", name, err)
		}
		return name, nil
	}

	var lastErr error
	for _, delimiter := range preferredMailboxDelimiters(parent, folders) {
		candidate := parent + delimiter + name
		if containsMailboxPath(folders, candidate) {
			return candidate, nil
		}
		if err := d.CreateFolder(candidate); err == nil {
			return candidate, nil
		} else {
			lastErr = err
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("imap create folder %q under %q: %w", name, parent, lastErr)
	}
	return "", fmt.Errorf("imap create folder %q under %q failed", name, parent)
}

func (c *APIClient) DeleteFolder(ctx context.Context, folder string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	folder = strings.TrimSpace(folder)
	if folder == "" {
		return errors.New("folder is required")
	}
	parent := mailboxParent(folder)
	if parent == "" {
		return errors.New("folder must have a parent mailbox")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}

	folders, err := d.GetFolders()
	if err != nil {
		return fmt.Errorf("imap list folders: %w", err)
	}
	for _, existing := range folders {
		clean := strings.TrimSpace(existing)
		if strings.EqualFold(clean, folder) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(folder+"/")) || strings.HasPrefix(strings.ToLower(clean), strings.ToLower(folder+".")) {
			return errors.New("folder has subfolders and cannot be deleted yet")
		}
	}

	if err := d.SelectFolder(folder); err != nil {
		return fmt.Errorf("imap select folder %q: %w", folder, err)
	}
	uids, err := d.GetUIDs("ALL")
	if err != nil {
		return fmt.Errorf("imap list folder messages %q: %w", folder, err)
	}
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.MoveEmail(uid, parent); err != nil {
			return fmt.Errorf("imap move uid %d from %q to %q: %w", uid, folder, parent, err)
		}
	}
	if err := d.SelectFolder(parent); err != nil {
		return fmt.Errorf("imap select parent folder %q: %w", parent, err)
	}
	if err := d.DeleteFolder(folder); err != nil {
		return fmt.Errorf("imap delete folder %q: %w", folder, err)
	}
	return nil
}

func (c *APIClient) RenameFolder(ctx context.Context, folder, name string) (string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", err
	}
	folder = strings.TrimSpace(folder)
	name = strings.TrimSpace(name)
	if folder == "" {
		return "", errors.New("folder is required")
	}
	if name == "" {
		return "", errors.New("folder name is required")
	}
	if strings.ContainsAny(name, "/.") {
		return "", errors.New("folder name must be a single level without / or .")
	}
	parent := mailboxParent(folder)
	if parent == "" {
		return "", errors.New("folder must have a parent mailbox")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return "", err
	}

	folders, err := d.GetFolders()
	if err != nil {
		return "", fmt.Errorf("imap list folders: %w", err)
	}
	delimiter := "/"
	if strings.Contains(folder, ".") {
		delimiter = "."
	}
	if !strings.Contains(folder, "/") && !strings.Contains(folder, ".") {
		for _, candidate := range preferredMailboxDelimiters(parent, folders) {
			delimiter = candidate
			break
		}
	}
	newPath := parent + delimiter + name
	if strings.EqualFold(folder, newPath) {
		return folder, nil
	}
	if containsMailboxPath(folders, newPath) {
		return "", fmt.Errorf("folder %q already exists", newPath)
	}
	if err := d.RenameFolder(folder, newPath); err != nil {
		return "", fmt.Errorf("imap rename folder %q to %q: %w", folder, newPath, err)
	}
	return newPath, nil
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
	c.opMu.Lock()
	defer c.opMu.Unlock()

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

func (c *APIClient) ApplyInboxAction(ctx context.Context, messageID, action, mailbox, targetMailbox string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(messageID))
	if err != nil || uid <= 0 {
		return fmt.Errorf("invalid message id %q", messageID)
	}
	action = strings.ToLower(strings.TrimSpace(action))
	targetMailbox = strings.TrimSpace(targetMailbox)

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox != "" && !strings.EqualFold(mailbox, c.mailbox) {
		if err := d.SelectFolder(mailbox); err != nil {
			return fmt.Errorf("imap select folder %q: %w", mailbox, err)
		}
	}

	moveToFolder := func(folder string) error {
		return ensureFolderThenRun(d, folder, func(folder string) error {
			return d.MoveEmail(uid, folder)
		})
	}

	isTrashMailbox := func(name string) bool {
		clean := strings.TrimSpace(strings.ToLower(name))
		return clean == "trash" || clean == "inbox/trash" || clean == "inbox.trash"
	}

	switch action {
	case "read":
		if err := d.MarkSeen(uid); err != nil {
			return fmt.Errorf("imap mark seen uid %d: %w", uid, err)
		}
		return nil
	case "archive":
		year := time.Now().Year()
		emails, err := d.GetEmails(uid)
		if err == nil {
			if email := emails[uid]; email != nil {
				ts := email.Sent
				if ts.IsZero() {
					ts = email.Received
				}
				if !ts.IsZero() {
					year = ts.UTC().Year()
				}
			}
		}
		archiveTargets := []string{fmt.Sprintf("Archive/%d", year), fmt.Sprintf("Archive.%d", year)}
		var lastErr error
		for _, folder := range archiveTargets {
			if err := moveToFolder(folder); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("imap move uid %d to yearly archive: %w", uid, lastErr)
		}
		return nil
	case "spam":
		if err := moveToFolder("Spam"); err != nil {
			return fmt.Errorf("imap move uid %d to Spam: %w", uid, err)
		}
		return nil
	case "delete":
		if isTrashMailbox(mailbox) {
			if err := d.DeleteEmail(uid); err != nil {
				return fmt.Errorf("imap delete uid %d from Trash: %w", uid, err)
			}
			return nil
		}
		trashTargets := []string{"Trash", "INBOX/Trash", "INBOX.Trash"}
		var lastErr error
		for _, folder := range trashTargets {
			if err := moveToFolder(folder); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("imap move uid %d to Trash: %w", uid, lastErr)
		}
		return nil
	case "move":
		if targetMailbox == "" {
			return errors.New("target mailbox is required")
		}
		if strings.EqualFold(strings.TrimSpace(mailbox), targetMailbox) {
			return nil
		}
		if err := moveToFolder(targetMailbox); err != nil {
			return fmt.Errorf("imap move uid %d to %q: %w", uid, targetMailbox, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported inbox action %q", action)
	}
}

// fetchAttachments pulls one message and returns its parsed attachments
// (go-imap's GetEmails decodes MIME parts into Email.Attachments).
func (c *APIClient) fetchAttachments(ctx context.Context, mailbox string, uid int) ([]goimap.Attachment, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uid <= 0 {
		return nil, fmt.Errorf("invalid message id %d", uid)
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox != "" && !strings.EqualFold(mailbox, c.mailbox) {
		if err := d.SelectFolder(mailbox); err != nil {
			return nil, fmt.Errorf("imap select folder %q: %w", mailbox, err)
		}
	}

	emails, err := d.GetEmails(uid)
	if err != nil {
		return nil, fmt.Errorf("imap fetch emails: %w", err)
	}
	e := emails[uid]
	if e == nil {
		return nil, fmt.Errorf("message %d not found in %q", uid, mailbox)
	}
	return e.Attachments, nil
}

func (c *APIClient) ListAttachments(ctx context.Context, mailbox string, uid int) ([]AttachmentInfo, error) {
	attachments, err := c.fetchAttachments(ctx, mailbox, uid)
	if err != nil {
		return nil, err
	}
	infos := make([]AttachmentInfo, 0, len(attachments))
	for i, a := range attachments {
		infos = append(infos, AttachmentInfo{
			Index:    i,
			Name:     a.Name,
			MimeType: a.MimeType,
			Size:     len(a.Content),
		})
	}
	return infos, nil
}

func (c *APIClient) GetAttachment(ctx context.Context, mailbox string, uid int, index int) (AttachmentInfo, []byte, error) {
	attachments, err := c.fetchAttachments(ctx, mailbox, uid)
	if err != nil {
		return AttachmentInfo{}, nil, err
	}
	if index < 0 || index >= len(attachments) {
		return AttachmentInfo{}, nil, ErrAttachmentNotFound
	}
	a := attachments[index]
	info := AttachmentInfo{
		Index:    index,
		Name:     a.Name,
		MimeType: a.MimeType,
		Size:     len(a.Content),
	}
	return info, a.Content, nil
}

// ensureFolderThenRun runs try against folder, creating the folder and retrying
// once if the first attempt fails (the folder commonly doesn't exist yet).
func ensureFolderThenRun(d *goimap.Dialer, folder string, try func(folder string) error) error {
	if err := try(folder); err == nil {
		return nil
	}
	if err := d.CreateFolder(folder); err != nil {
		return err
	}
	return try(folder)
}

func (c *APIClient) saveMessage(ctx context.Context, draft DraftMessage, targets []string, flags []string, failureVerb string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if len(draft.To) == 0 {
		return errors.New("at least one TO recipient is required")
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}

	raw := mailmsg.Message{
		From:        c.username,
		To:          draft.To,
		CC:          draft.CC,
		BCC:         draft.BCC,
		Subject:     draft.Subject,
		Body:        draft.Body,
		Mode:        draft.Mode,
		Attachments: draft.Attachments,
	}.Build()

	var lastErr error
	for _, folder := range targets {
		err := ensureFolderThenRun(d, folder, func(folder string) error {
			return d.Append(folder, flags, time.Now(), raw)
		})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return fmt.Errorf("failed to %s: %w", failureVerb, lastErr)
	}
	return fmt.Errorf("failed to %s", failureVerb)
}

func (c *APIClient) SaveDraft(ctx context.Context, draft DraftMessage) error {
	return c.saveMessage(ctx, draft, []string{"Drafts", "INBOX/Drafts", "INBOX.Drafts"}, []string{"\\Draft"}, "save draft")
}

func (c *APIClient) SaveSent(ctx context.Context, draft DraftMessage) error {
	targets := []string{"Sent", "INBOX/Sent", "INBOX.Sent", "Sent Items", "INBOX/Sent Items", "INBOX.Sent Items"}
	return c.saveMessage(ctx, draft, targets, []string{"\\Seen"}, "save sent mail")
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
