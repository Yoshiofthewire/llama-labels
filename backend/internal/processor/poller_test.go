package processor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imapadapter "kypost-server/backend/internal/adapters/imap"
	"kypost-server/backend/internal/config"
	"kypost-server/backend/internal/logging"
	"kypost-server/backend/internal/rules"
	"kypost-server/backend/internal/state"
)

// TestMailCacheEntriesFromMessages covers the pure conversion tickUser uses
// to opportunistically warm the mail cache with what ListUnreadInbox just
// fetched for classification (poller.go). Full tickUser integration
// (constructing a Poller against a fake IMAP dialer) isn't covered here —
// this codebase has no fake-goimap-Dialer test infrastructure, matching the
// same gap noted for adapters/imap's ListOverviews/GetMessageBodies.
func TestMailCacheEntriesFromMessages(t *testing.T) {
	messages := []imapadapter.Message{
		{
			ID: "42", Subject: "Invoice", Sender: "alice@example.com", SentTo: "me@example.com",
			CC: "cc@example.com", BCC: "bcc@example.com", Keywords: []string{"Work"},
			AtUTC: "2026-01-01T00:00:00Z", Body: "the body",
		},
		// Malformed IDs (shouldn't happen in practice, since imap.Message.ID
		// is always strconv.Itoa(uid)) must be skipped, not panic or produce
		// a garbage UID.
		{ID: "not-a-number", Subject: "bad"},
	}

	entries := mailCacheEntriesFromMessages(messages)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (malformed ID skipped), got %d: %+v", len(entries), entries)
	}

	e := entries[0]
	if e.UID != 42 || e.MessageID != "42" {
		t.Fatalf("expected uid/messageId 42, got %+v", e)
	}
	if e.Subject != "Invoice" || e.Sender != "alice@example.com" || e.SentTo != "me@example.com" {
		t.Fatalf("expected envelope fields carried over, got %+v", e)
	}
	if e.CC != "cc@example.com" || e.BCC != "bcc@example.com" {
		t.Fatalf("expected CC/BCC carried over, got %+v", e)
	}
	if len(e.Keywords) != 1 || e.Keywords[0] != "Work" {
		t.Fatalf("expected keywords carried over, got %+v", e.Keywords)
	}
	if e.Body != "the body" {
		t.Fatalf("expected body carried over so the classic cache-first path can serve it, got %q", e.Body)
	}
	// ListUnreadInbox only ever returns messages matching an IMAP UNSEEN
	// search, so Status is always "unread" regardless of flags.
	if e.Status != "unread" {
		t.Fatalf("expected status always unread, got %q", e.Status)
	}
}

func TestMailCacheEntriesFromMessages_EmptyInput(t *testing.T) {
	entries := mailCacheEntriesFromMessages(nil)
	if len(entries) != 0 {
		t.Fatalf("expected no entries for empty input, got %+v", entries)
	}
}

func TestBuildNativeNotificationText(t *testing.T) {
	tests := []struct {
		name      string
		msg       imapadapter.Message
		wantTitle string
		wantBody  string
	}{
		{
			name:      "sender and subject",
			msg:       imapadapter.Message{Sender: "alice@example.com", Subject: "Invoice #42"},
			wantTitle: "alice@example.com",
			wantBody:  "Invoice #42",
		},
		{
			name:      "missing subject",
			msg:       imapadapter.Message{Sender: "bob@example.com"},
			wantTitle: "bob@example.com",
			wantBody:  "You have a new email.",
		},
		{
			name:      "missing sender",
			msg:       imapadapter.Message{Subject: "Meeting notes"},
			wantTitle: "New Email",
			wantBody:  "Meeting notes",
		},
		{
			name:      "empty message",
			msg:       imapadapter.Message{},
			wantTitle: "New Email",
			wantBody:  "You have a new email.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, body := buildNativeNotificationText(tc.msg)
			if title != tc.wantTitle || body != tc.wantBody {
				t.Fatalf("buildNativeNotificationText() = (%q, %q), want (%q, %q)", title, body, tc.wantTitle, tc.wantBody)
			}
		})
	}
}

func TestBuildNativePushData(t *testing.T) {
	tests := []struct {
		name     string
		msg      imapadapter.Message
		keywords []string
		title    string
		body     string
		want     map[string]string
	}{
		{
			name:     "populated message and keywords",
			msg:      imapadapter.Message{ID: " 123 ", Sender: " alice@example.com ", Subject: " Invoice #42 "},
			keywords: []string{"work", "billing"},
			title:    "alice@example.com",
			body:     "Invoice #42",
			want: map[string]string{
				"messageId":    "123",
				"sender":       "alice@example.com",
				"subject":      "Invoice #42",
				"senderName":   "alice@example.com",
				"emailSubject": "Invoice #42",
				"Keywords":     "work,billing",
				"title":        "alice@example.com",
				"body":         "Invoice #42",
				"url":          "/read",
			},
		},
		{
			name:     "nil keywords produce empty string, not panic",
			msg:      imapadapter.Message{ID: "1", Sender: "bob@example.com", Subject: "Hi"},
			keywords: nil,
			title:    "bob@example.com",
			body:     "Hi",
			want: map[string]string{
				"messageId":    "1",
				"sender":       "bob@example.com",
				"subject":      "Hi",
				"senderName":   "bob@example.com",
				"emailSubject": "Hi",
				"Keywords":     "",
				"title":        "bob@example.com",
				"body":         "Hi",
				"url":          "/read",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildNativePushData(tc.msg, tc.keywords, tc.title, tc.body)
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("buildNativePushData()[%q] = %q, want %q", key, got[key], want)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("buildNativePushData() has %d keys, want %d: %v", len(got), len(tc.want), got)
			}
		})
	}
}

func TestShouldSendNotification(t *testing.T) {
	tests := []struct {
		name          string
		settings      config.UserNotificationSettings
		selectedLabel string
		keywords      []string
		want          bool
	}{
		{
			name:     "none mode never sends",
			settings: config.UserNotificationSettings{Mode: "none", Keywords: []string{"Urgent"}},
			want:     false,
		},
		{
			name:     "all mode always sends",
			settings: config.UserNotificationSettings{Mode: "all"},
			want:     true,
		},
		{
			name:          "keywords mode matches selected label",
			settings:      config.UserNotificationSettings{Mode: "keywords", Keywords: []string{"Urgent"}},
			selectedLabel: "urgent",
			want:          true,
		},
		{
			name:     "keywords mode matches mapped keyword",
			settings: config.UserNotificationSettings{Mode: "keywords", Keywords: []string{"billing"}},
			keywords: []string{"Invoices", "Billing"},
			want:     true,
		},
		{
			name:          "keywords mode does not match when nothing selected",
			settings:      config.UserNotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "support",
			keywords:      []string{"helpdesk"},
			want:          false,
		},
		{
			name:          "keywords mode does not send when uncategorized",
			settings:      config.UserNotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "",
			keywords:      nil,
			want:          false,
		},
		{
			name:          "keywords mode sends from selected label before mailbox keyword readback",
			settings:      config.UserNotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "urgent",
			keywords:      nil,
			want:          true,
		},
		{
			name:          "all mode sends even when uncategorized",
			settings:      config.UserNotificationSettings{Mode: "all"},
			selectedLabel: "",
			keywords:      nil,
			want:          true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSendNotification(tc.settings, tc.selectedLabel, tc.keywords); got != tc.want {
				t.Fatalf("shouldSendNotification() = %v, want %v", got, tc.want)
			}
		})
	}
}

// noopMailClient is a minimal imapadapter.Client fake for handleMessage
// tests — only the methods rules.ApplyOutcome might call do anything
// observable; everything else is an unused no-op to satisfy the interface.
// inboxActionErr, when set, is returned by ApplyInboxAction so tests can
// inject a genuine action failure (e.g. a transient IMAP error on
// archive/move/delete) instead of always succeeding.
type noopMailClient struct {
	appliedLabels  []string
	inboxActions   []string
	inboxActionErr error
}

func (c *noopMailClient) ListUnreadInbox(context.Context, string) ([]imapadapter.Message, string, error) {
	return nil, "", nil
}
func (c *noopMailClient) ListUnreadMessages(context.Context, string, int) ([]imapadapter.UnreadMessage, error) {
	return nil, nil
}
func (c *noopMailClient) ListOverviews(context.Context, string, int) ([]imapadapter.Overview, error) {
	return nil, nil
}
func (c *noopMailClient) SearchMessages(context.Context, string, string, string, int) ([]imapadapter.Overview, error) {
	return nil, nil
}
func (c *noopMailClient) GetMessageBodies(context.Context, string, []int) (map[int]imapadapter.MessageContent, error) {
	return nil, nil
}
func (c *noopMailClient) ListLabels(context.Context) ([]string, error)             { return nil, nil }
func (c *noopMailClient) ListSubfolders(context.Context, string) ([]string, error) { return nil, nil }
func (c *noopMailClient) CreateFolder(context.Context, string, string) (string, error) {
	return "", nil
}
func (c *noopMailClient) RenameFolder(context.Context, string, string) (string, error) {
	return "", nil
}
func (c *noopMailClient) DeleteFolder(context.Context, string) error { return nil }
func (c *noopMailClient) EnsureLabel(context.Context, string) error  { return nil }
func (c *noopMailClient) ApplyLabel(_ context.Context, _ string, label string) error {
	c.appliedLabels = append(c.appliedLabels, label)
	return nil
}
func (c *noopMailClient) RemoveLabel(context.Context, string, string) error { return nil }
func (c *noopMailClient) ApplyInboxAction(_ context.Context, _ string, action, _, _ string) error {
	c.inboxActions = append(c.inboxActions, action)
	return c.inboxActionErr
}
func (c *noopMailClient) ListAttachments(context.Context, string, int) ([]imapadapter.AttachmentInfo, error) {
	return nil, nil
}
func (c *noopMailClient) GetAttachment(context.Context, string, int, int) (imapadapter.AttachmentInfo, []byte, error) {
	return imapadapter.AttachmentInfo{}, nil, nil
}
func (c *noopMailClient) SaveDraft(context.Context, imapadapter.DraftMessage) error { return nil }
func (c *noopMailClient) SaveSent(context.Context, imapadapter.DraftMessage) error  { return nil }

// TestHandleMessage_StopRuleShortCircuitsClassification proves a matched
// "stop" rule skips classifyWithRetry entirely, rather than merely skipping
// its result: the Poller's classifier field is left nil, so if handleMessage
// called classifyWithRetry anyway, HTTPClient.Classify would panic on a nil
// receiver dereference (c.baseURL inside ensureWarm) and fail this test.
// The message must still be marked processed and recorded as a Decision.
func TestHandleMessage_StopRuleShortCircuitsClassification(t *testing.T) {
	logger, err := logging.New(t.TempDir())
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	p := &Poller{log: logger} // classifier intentionally left nil

	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	mail := &noopMailClient{}
	uc := userCtx{
		id:   "user-1",
		mail: mail,
		rules: []rules.Rule{
			{
				Name:    "archive and stop",
				Enabled: true,
				Match: rules.MatchGroup{
					Op:         "allof",
					Conditions: []rules.Condition{{Field: "subject", Comparator: "contains", Value: "newsletter"}},
				},
				Actions: []rules.Action{{Type: "archive"}, {Type: "stop"}},
			},
		},
		store: store,
	}
	msg := imapadapter.Message{ID: "42", Subject: "Weekly newsletter", Sender: "news@example.com"}

	if err := p.handleMessage(context.Background(), uc, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	if len(mail.inboxActions) != 1 || mail.inboxActions[0] != "archive" {
		t.Fatalf("expected the archive action to be applied, got %+v", mail.inboxActions)
	}
	if !store.Seen(msg.ID) {
		t.Fatal("expected the message to be marked processed")
	}
	decisions := store.Decisions(10)
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision recorded, got %d: %+v", len(decisions), decisions)
	}
	if decisions[0].Status != "applied" || decisions[0].Detail != "rule(s) applied: archive and stop" {
		t.Fatalf("unexpected decision recorded: %+v", decisions[0])
	}
}

// TestHandleMessage_StopRuleActionFailureIsSurfaced proves a genuine action
// failure (e.g. a transient IMAP error on the archive call) is logged and
// reflected in the recorded Decision's Detail, rather than being silently
// treated as success — the bug this test guards against left the message
// permanently marked processed with no record anywhere that the archive
// never actually happened.
func TestHandleMessage_StopRuleActionFailureIsSurfaced(t *testing.T) {
	logDir := t.TempDir()
	logger, err := logging.New(logDir)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	p := &Poller{log: logger} // classifier intentionally left nil

	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	wantErr := "imap: connection reset by peer"
	mail := &noopMailClient{inboxActionErr: errors.New(wantErr)}
	uc := userCtx{
		id:   "user-1",
		mail: mail,
		rules: []rules.Rule{
			{
				Name:    "archive and stop",
				Enabled: true,
				Match: rules.MatchGroup{
					Op:         "allof",
					Conditions: []rules.Condition{{Field: "subject", Comparator: "contains", Value: "newsletter"}},
				},
				Actions: []rules.Action{{Type: "archive"}, {Type: "stop"}},
			},
		},
		store: store,
	}
	msg := imapadapter.Message{ID: "42", Subject: "Weekly newsletter", Sender: "news@example.com"}

	if err := p.handleMessage(context.Background(), uc, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// The failed archive doesn't stop control flow — stop still short-circuits
	// classification and the message is still marked processed (design is
	// unchanged); what must change is that the failure is observable.
	if !store.Seen(msg.ID) {
		t.Fatal("expected the message to still be marked processed (Stopped control flow unchanged)")
	}

	decisions := store.Decisions(10)
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision recorded, got %d: %+v", len(decisions), decisions)
	}
	d := decisions[0]
	if !strings.Contains(d.Detail, "rule(s) applied: archive and stop") {
		t.Fatalf("expected Detail to still report the matched rule, got %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "1 action(s) failed") || !strings.Contains(d.Detail, wantErr) {
		t.Fatalf("expected Detail to mention the failed action and its error, got %q", d.Detail)
	}

	logBytes, err := os.ReadFile(filepath.Join(logDir, "app.log"))
	if err != nil {
		t.Fatalf("reading app.log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "rule action failed") {
		t.Fatalf("expected an ERROR log line for the failed action, got log:\n%s", logText)
	}
	if !strings.Contains(logText, wantErr) {
		t.Fatalf("expected the log line to include the underlying error, got log:\n%s", logText)
	}
	if !strings.Contains(logText, "user-1") || !strings.Contains(logText, "42") {
		t.Fatalf("expected the log line to include user_id and message_id, got log:\n%s", logText)
	}
}

// TestHandleMessage_NonMatchingRuleStillClassifies is the mirror check:
// when no rule matches, handleMessage must still reach classification. It
// can't call the real Ollama HTTP path in a unit test, so it asserts the
// weaker but still meaningful property that a nil classifier client *does*
// panic once rule evaluation is out of the way — proving the earlier
// no-panic result above was actually caused by the stop short-circuit and
// not by some unrelated reason classifyWithRetry never runs.
func TestHandleMessage_NonMatchingRuleStillClassifies(t *testing.T) {
	logger, err := logging.New(t.TempDir())
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	p := &Poller{log: logger} // classifier intentionally left nil

	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	uc := userCtx{
		id:               "user-1",
		mail:             &noopMailClient{},
		autoLabelEnabled: true,
		rules: []rules.Rule{
			{
				Name:    "never matches",
				Enabled: true,
				Match: rules.MatchGroup{
					Op:         "allof",
					Conditions: []rules.Condition{{Field: "subject", Comparator: "contains", Value: "no-such-substring"}},
				},
				Actions: []rules.Action{{Type: "archive"}},
			},
		},
		store: store,
	}
	msg := imapadapter.Message{ID: "43", Subject: "Ordinary mail", Sender: "someone@example.com"}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected classifyWithRetry to be reached (and panic on the nil classifier client) when no rule matches")
		}
	}()
	_ = p.handleMessage(context.Background(), uc, msg)
}

// TestHandleMessage_AutoLabelDisabledUsesConfiguredLabel proves the
// auto-labeling-disabled fallback always applies a label present in the
// account's configured allowlist, rather than the hardcoded literal
// "Primary" — which silently drops mail into the invisible Uncategorized
// tab (server.go's bucket()/firstMatchingKeyword) whenever "Primary" isn't
// one of the user's configured labels, making new mail look archived and
// unsorted from the frontend's perspective (ReadPage.tsx always defaults
// activeTab to tabs[0], never to Uncategorized).
func TestHandleMessage_AutoLabelDisabledUsesConfiguredLabel(t *testing.T) {
	logger, err := logging.New(t.TempDir())
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	p := &Poller{log: logger} // classifier intentionally left nil; disabled path never calls it
	p.cfg.Labels.Allowlist = []string{"Work", "Bills"}

	mail := &noopMailClient{}
	uc := userCtx{
		id:               "user-1",
		mail:             mail,
		store:            store,
		autoLabelEnabled: false,
	}
	msg := imapadapter.Message{ID: "44", Subject: "Invoice due", Sender: "billing@example.com"}

	if err := p.handleMessage(context.Background(), uc, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	if len(mail.appliedLabels) == 0 {
		t.Fatal("expected a label to be applied")
	}
	got := mail.appliedLabels[0]
	found := false
	for _, allowed := range p.cfg.Labels.Allowlist {
		if strings.EqualFold(allowed, got) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("applied label %q is not in the configured allowlist %v — mail will land in the invisible Uncategorized tab", got, p.cfg.Labels.Allowlist)
	}
}
