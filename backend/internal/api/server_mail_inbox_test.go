package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	imapadapter "llama-lab/backend/internal/adapters/imap"
	"llama-lab/backend/internal/config"
	"llama-lab/backend/internal/mailcache"
	"llama-lab/backend/internal/mailmsg"
)

// fakeMailClient is a configurable imapadapter.Client for exercising
// serveInbox's cache-first classic path and since-based delta path without
// a real IMAP connection. Every method call is counted so tests can assert
// IMAP was (or wasn't) touched.
type fakeMailClient struct {
	unread      []imapadapter.UnreadMessage
	unreadErr   error
	unreadCalls int

	overviews     []imapadapter.Overview
	overviewsErr  error
	overviewCalls int

	bodies             map[int]string
	bodyHasAttachments map[int]bool
	bodiesErr          error
	bodiesCalls        int
	lastBodyUIDs       []int

	attachments    map[int][]mailmsg.Attachment
	attachmentsErr error
}

func (f *fakeMailClient) ListUnreadInbox(_ context.Context, _ string) ([]imapadapter.Message, string, error) {
	return nil, "", nil
}

func (f *fakeMailClient) ListUnreadMessages(_ context.Context, _ string, _ int) ([]imapadapter.UnreadMessage, error) {
	f.unreadCalls++
	return f.unread, f.unreadErr
}

func (f *fakeMailClient) ListOverviews(_ context.Context, _ string, _ int) ([]imapadapter.Overview, error) {
	f.overviewCalls++
	return f.overviews, f.overviewsErr
}

func (f *fakeMailClient) GetMessageBodies(_ context.Context, _ string, uids []int) (map[int]imapadapter.MessageContent, error) {
	f.bodiesCalls++
	f.lastBodyUIDs = append([]int{}, uids...)
	if f.bodiesErr != nil {
		return nil, f.bodiesErr
	}
	out := map[int]imapadapter.MessageContent{}
	for _, uid := range uids {
		if b, ok := f.bodies[uid]; ok {
			out[uid] = imapadapter.MessageContent{Body: b, HasAttachments: f.bodyHasAttachments[uid]}
		}
	}
	return out, nil
}

func (f *fakeMailClient) ListLabels(_ context.Context) ([]string, error) { return nil, nil }
func (f *fakeMailClient) ListSubfolders(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeMailClient) CreateFolder(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}
func (f *fakeMailClient) RenameFolder(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}
func (f *fakeMailClient) DeleteFolder(_ context.Context, _ string) error         { return nil }
func (f *fakeMailClient) EnsureLabel(_ context.Context, _ string) error          { return nil }
func (f *fakeMailClient) ApplyLabel(_ context.Context, _ string, _ string) error { return nil }
func (f *fakeMailClient) ApplyInboxAction(_ context.Context, _ string, _ string, _ string, _ string) error {
	return nil
}
func (f *fakeMailClient) SaveDraft(_ context.Context, _ imapadapter.DraftMessage) error { return nil }
func (f *fakeMailClient) SaveSent(_ context.Context, _ imapadapter.DraftMessage) error  { return nil }

// Attachment fixtures for the /api/mail/attachment(s) handler tests.
func (f *fakeMailClient) ListAttachments(_ context.Context, _ string, uid int) ([]imapadapter.AttachmentInfo, error) {
	if f.attachmentsErr != nil {
		return nil, f.attachmentsErr
	}
	infos := make([]imapadapter.AttachmentInfo, 0, len(f.attachments[uid]))
	for i, a := range f.attachments[uid] {
		infos = append(infos, imapadapter.AttachmentInfo{
			Index: i, Name: a.Name, MimeType: a.MimeType, Size: len(a.Content),
		})
	}
	return infos, nil
}

func (f *fakeMailClient) GetAttachment(_ context.Context, _ string, uid int, index int) (imapadapter.AttachmentInfo, []byte, error) {
	if f.attachmentsErr != nil {
		return imapadapter.AttachmentInfo{}, nil, f.attachmentsErr
	}
	attachments := f.attachments[uid]
	if index < 0 || index >= len(attachments) {
		return imapadapter.AttachmentInfo{}, nil, imapadapter.ErrAttachmentNotFound
	}
	a := attachments[index]
	info := imapadapter.AttachmentInfo{
		Index: index, Name: a.Name, MimeType: a.MimeType, Size: len(a.Content),
	}
	return info, a.Content, nil
}

func testInboxCache(t *testing.T) *mailcache.Store {
	t.Helper()
	cache, err := mailcache.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailcache.New: %v", err)
	}
	return cache
}

type inboxResponse struct {
	Tabs    []string                `json:"tabs"`
	ByTab   map[string][]inboxEmail `json:"byTab"`
	Delta   bool                    `json:"delta"`
	Cursor  int64                   `json:"cursor"`
	Removed []string                `json:"removed"`
}

func decodeInboxResponse(t *testing.T, rec *httptest.ResponseRecorder) inboxResponse {
	t.Helper()
	var resp inboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

func allEmails(resp inboxResponse) []inboxEmail {
	var out []inboxEmail
	for _, entries := range resp.ByTab {
		out = append(out, entries...)
	}
	return out
}

func TestServeInbox_ClassicServedFromWarmCache(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	if err := cache.Upsert("INBOX", []mailcache.Entry{
		{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "body-1"},
		{UID: 2, MessageID: "2", Subject: "b", Sender: "b@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "body-2"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	fake := &fakeMailClient{}
	rec := httptest.NewRecorder()
	srv.serveInbox(rec, context.Background(), fake, cache, cfg, "", 2, 0, false)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if fake.unreadCalls != 0 || fake.overviewCalls != 0 || fake.bodiesCalls != 0 {
		t.Fatalf("expected zero IMAP calls when cache is fully warmed, got unread=%d overviews=%d bodies=%d", fake.unreadCalls, fake.overviewCalls, fake.bodiesCalls)
	}

	resp := decodeInboxResponse(t, rec)
	if resp.Delta {
		t.Fatalf("classic response must not set delta=true")
	}
	emails := allEmails(resp)
	if len(emails) != 2 {
		t.Fatalf("expected 2 emails from cache, got %d", len(emails))
	}
	for _, e := range emails {
		if e.Body == "" {
			t.Fatalf("expected cached body to be served, got empty body for %+v", e)
		}
		if e.ChangeType != "" {
			t.Fatalf("classic response entries must not set changeType, got %+v", e)
		}
	}
}

func TestServeInbox_ClassicFallsBackAndSelfWarms(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	fake := &fakeMailClient{unread: []imapadapter.UnreadMessage{
		{MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "body-1"},
	}}

	rec1 := httptest.NewRecorder()
	srv.serveInbox(rec1, context.Background(), fake, cache, cfg, "", 1, 0, false)
	if rec1.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec1.Code, rec1.Body.String())
	}
	if fake.unreadCalls != 1 {
		t.Fatalf("expected exactly one live fetch on a cold cache, got %d", fake.unreadCalls)
	}

	// Second call for the same mailbox+limit should now be servable from
	// the self-warmed cache, with no further live fetch.
	rec2 := httptest.NewRecorder()
	srv.serveInbox(rec2, context.Background(), fake, cache, cfg, "", 1, 0, false)
	if fake.unreadCalls != 1 {
		t.Fatalf("expected no additional live fetch after self-warming, got %d total calls", fake.unreadCalls)
	}
	resp := decodeInboxResponse(t, rec2)
	emails := allEmails(resp)
	if len(emails) != 1 || emails[0].Body != "body-1" {
		t.Fatalf("expected the self-warmed body to be served, got %+v", emails)
	}
}

func TestServeInbox_DeltaFirstCallAllNew(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
		},
		bodies: map[int]string{1: "body-1"},
	}

	rec := httptest.NewRecorder()
	srv.serveInbox(rec, context.Background(), fake, cache, cfg, "", 10, 0, true)

	if fake.overviewCalls != 1 {
		t.Fatalf("expected exactly one overview fetch, got %d", fake.overviewCalls)
	}
	if fake.bodiesCalls != 1 {
		t.Fatalf("expected exactly one body fetch for the new message, got %d", fake.bodiesCalls)
	}

	resp := decodeInboxResponse(t, rec)
	if !resp.Delta {
		t.Fatalf("expected delta=true")
	}
	if resp.Cursor == 0 {
		t.Fatalf("expected a non-zero cursor")
	}
	if len(resp.Removed) != 0 {
		t.Fatalf("expected no removed entries on first sync, got %+v", resp.Removed)
	}
	emails := allEmails(resp)
	if len(emails) != 1 || emails[0].ChangeType != "new" || emails[0].Body != "body-1" {
		t.Fatalf("expected one new entry with body populated, got %+v", emails)
	}
}

func TestServeInbox_DeltaFlagChangeIsUpdatedWithoutRefetchingBody(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
		},
		bodies: map[int]string{1: "body-1"},
	}
	rec1 := httptest.NewRecorder()
	srv.serveInbox(rec1, context.Background(), fake, cache, cfg, "", 10, 0, true)
	first := decodeInboxResponse(t, rec1)

	// Second poll: the message's status flipped to read. The client's
	// cursor is the one returned by the first call.
	fake.overviews = []imapadapter.Overview{
		{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "read", AtUTC: "2026-01-01T00:00:00Z"},
	}
	rec2 := httptest.NewRecorder()
	srv.serveInbox(rec2, context.Background(), fake, cache, cfg, "", 10, first.Cursor, true)

	if fake.bodiesCalls != 1 {
		t.Fatalf("expected no additional body fetch for an already-known message, got %d total body fetch calls", fake.bodiesCalls)
	}
	resp := decodeInboxResponse(t, rec2)
	emails := allEmails(resp)
	if len(emails) != 1 || emails[0].ChangeType != "updated" || emails[0].Status != "read" {
		t.Fatalf("expected one updated entry with status=read, got %+v", emails)
	}
	if emails[0].Body != "" {
		t.Fatalf("expected no body on an updated entry (client already has it), got %q", emails[0].Body)
	}
}

func TestServeInbox_DeltaSkipsBodyFetchWhenAlreadyWarmed(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	// Simulate the daemon having already warmed uid 1's body before any
	// client ever polls.
	if err := cache.Upsert("INBOX", []mailcache.Entry{
		{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "warmed-body"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
		},
	}
	rec := httptest.NewRecorder()
	srv.serveInbox(rec, context.Background(), fake, cache, cfg, "", 10, 0, true)

	if fake.bodiesCalls != 0 {
		t.Fatalf("expected no body fetch when the daemon already warmed the body, got %d calls, uids=%v", fake.bodiesCalls, fake.lastBodyUIDs)
	}
	resp := decodeInboxResponse(t, rec)
	emails := allEmails(resp)
	if len(emails) != 1 || emails[0].ChangeType != "new" || emails[0].Body != "warmed-body" {
		t.Fatalf("expected the warmed body to be served for the new entry, got %+v", emails)
	}
}

func TestServeInbox_DeltaWindowFalloutReportedAsRemoved(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()

	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{UID: 1, MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
			{UID: 2, MessageID: "2", Subject: "b", Sender: "b@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
		},
		bodies: map[int]string{1: "body-1", 2: "body-2"},
	}
	rec1 := httptest.NewRecorder()
	srv.serveInbox(rec1, context.Background(), fake, cache, cfg, "", 10, 0, true)
	first := decodeInboxResponse(t, rec1)

	// uid 1 ages out of the window.
	fake.overviews = []imapadapter.Overview{
		{UID: 2, MessageID: "2", Subject: "b", Sender: "b@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
	}
	rec2 := httptest.NewRecorder()
	srv.serveInbox(rec2, context.Background(), fake, cache, cfg, "", 10, first.Cursor, true)

	resp := decodeInboxResponse(t, rec2)
	if len(resp.Removed) != 1 || resp.Removed[0] != "1" {
		t.Fatalf("expected uid 1 reported as removed, got %+v", resp.Removed)
	}
}

func TestServeInbox_TabBucketingByKeyword(t *testing.T) {
	srv := newTestServer(t)
	cache := testInboxCache(t)
	cfg := config.Default()
	cfg.Labels.Allowlist = []string{"Work"}

	fake := &fakeMailClient{unread: []imapadapter.UnreadMessage{
		{MessageID: "1", Subject: "a", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "b1", Keywords: []string{"Work"}},
		{MessageID: "2", Subject: "b", Sender: "b@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "b2"},
	}}

	rec := httptest.NewRecorder()
	srv.serveInbox(rec, context.Background(), fake, cache, cfg, "", 10, 0, false)
	resp := decodeInboxResponse(t, rec)

	if len(resp.ByTab["Work"]) != 1 || resp.ByTab["Work"][0].MessageID != "1" {
		t.Fatalf("expected message 1 bucketed into Work tab, got %+v", resp.ByTab["Work"])
	}
	if len(resp.ByTab[inboxUncategorizedTab]) != 1 || resp.ByTab[inboxUncategorizedTab][0].MessageID != "2" {
		t.Fatalf("expected message 2 bucketed into Uncategorized, got %+v", resp.ByTab[inboxUncategorizedTab])
	}
}
