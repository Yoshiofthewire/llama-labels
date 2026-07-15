# PGP QR Contact Card Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user flag one contact in their own address book as "me," and have that contact's full details ride along with the existing PGP QR key-exchange response so scanning someone's PGP QR code also hands over their basic contact card.

**Architecture:** Add an `IsSelf` flag to the existing `contacts.Contact` model (no new storage) with store-level uniqueness enforcement, expose it through a new `POST /api/contacts/{id}/self` toggle endpoint, fold the flagged contact's shareable fields into `handlePGPQRKey`'s existing JSON response under a new `contactCard` key, and surface an entry point for all of this in the existing Contacts and Security pages — no new form, since the existing contact-edit UI already covers every field.

**Tech Stack:** Go 1.22+ (`net/http` `ServeMux` path values), React 18 + TypeScript (Vite), existing `contacts.Store` JSON-file persistence.

## Global Constraints

- Backend build: `cd backend && go build -buildvcs=false ./...` must succeed with zero errors.
- Backend vet: `go vet ./...` must pass.
- Backend tests: `cd backend && go test ./...` must pass.
- Frontend build: `cd frontend && npm run build` must succeed with zero TypeScript errors.
- No new dependencies without explicit approval (none are needed for this feature).
- Any new sync-relevant field on `contacts.Contact` must participate in `Contact.tombstone()`'s clear-list if leaving it set on a tombstone would be wrong (see Task 1).
- Full field/scope reference: `docs/superpowers/specs/2026-07-15-pgp-qr-contact-card-design.md`.

---

### Task 1: `IsSelf` flag on the contacts data model

**Files:**
- Modify: `backend/internal/contacts/contacts.go` (the `Contact` struct, `tombstone()`)
- Modify: `backend/internal/contacts/store.go` (`Upsert`, plus two new methods)
- Test: `backend/internal/contacts/self_test.go` (new file)

**Interfaces:**
- Consumes: nothing new — builds directly on the existing `Contact`, `Store.Upsert(c Contact) (Contact, error)`, `Store.Get(uid string) (Contact, bool)`, `Store.Delete(uid string) (bool, error)`.
- Produces: `Contact.IsSelf bool` (json tag `isSelf,omitempty`); `Store.SetSelf(uid string, self bool) (Contact, bool, error)` (returns the updated contact, whether `uid` was found, and any I/O error); `Store.GetSelf() (Contact, bool)` (the live contact with `IsSelf == true`, if any). Task 2 and Task 3 call these two new methods by name.

- [ ] **Step 1: Write the failing tests**

Create `backend/internal/contacts/self_test.go`:

```go
package contacts

import "testing"

func TestSetSelf_MarksAndEnforcesUniqueness(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Upsert(Contact{FormattedName: "Alice"})
	b, _ := s.Upsert(Contact{FormattedName: "Bob"})

	if _, ok := s.GetSelf(); ok {
		t.Fatal("expected no self-contact before SetSelf")
	}

	updated, found, err := s.SetSelf(a.UID, true)
	if err != nil || !found {
		t.Fatalf("SetSelf(a, true): found=%v err=%v", found, err)
	}
	if !updated.IsSelf {
		t.Fatal("expected IsSelf true on returned contact")
	}
	self, ok := s.GetSelf()
	if !ok || self.UID != a.UID {
		t.Fatalf("GetSelf: ok=%v uid=%q, want %q", ok, self.UID, a.UID)
	}

	// Marking b as self must clear a's flag — at most one self-contact ever.
	if _, found, err := s.SetSelf(b.UID, true); err != nil || !found {
		t.Fatalf("SetSelf(b, true): found=%v err=%v", found, err)
	}
	self, ok = s.GetSelf()
	if !ok || self.UID != b.UID {
		t.Fatalf("GetSelf after re-marking: ok=%v uid=%q, want %q", ok, self.UID, b.UID)
	}
	refreshedA, _ := s.Get(a.UID)
	if refreshedA.IsSelf {
		t.Fatal("expected a's IsSelf to be cleared after b was marked self")
	}
}

func TestSetSelf_UnknownUIDReturnsNotFound(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := s.SetSelf("nope", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false for unknown uid")
	}
}

func TestSetSelf_FalseClearsFlag(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Upsert(Contact{FormattedName: "Alice"})
	if _, _, err := s.SetSelf(a.UID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetSelf(a.UID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSelf(); ok {
		t.Fatal("expected no self-contact after unmarking")
	}
}

// TestUpsert_PreservesIsSelfAcrossEdits guards the exact bug this feature
// would otherwise reintroduce: the API's contactPayload/toContact() (see
// api/contacts_handlers.go) never carries isSelf, so every normal edit through
// PUT /api/contacts/{id} builds a fresh Contact with IsSelf false. Upsert
// must restore the existing record's IsSelf, the same way it already
// restores CreatedAt, or editing any field of your own contact card would
// silently un-mark it.
func TestUpsert_PreservesIsSelfAcrossEdits(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Upsert(Contact{FormattedName: "Alice"})
	if _, _, err := s.SetSelf(a.UID, true); err != nil {
		t.Fatal(err)
	}
	edited, _ := s.Get(a.UID)
	edited.Title = "Engineer"
	edited.IsSelf = false // what toContact() would produce on a normal edit
	updated, err := s.Upsert(edited)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.IsSelf {
		t.Fatal("expected Upsert to preserve IsSelf from the existing record")
	}
}

func TestDelete_ClearsIsSelf(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Upsert(Contact{FormattedName: "Alice"})
	if _, _, err := s.SetSelf(a.UID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(a.UID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSelf(); ok {
		t.Fatal("expected no self-contact after deleting the self-contact")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd backend && go test ./internal/contacts/... -run 'TestSetSelf|TestUpsert_PreservesIsSelfAcrossEdits|TestDelete_ClearsIsSelf' -v`
Expected: build failure — `s.SetSelf undefined`, `s.GetSelf undefined`, `edited.IsSelf undefined` (none of these exist yet).

- [ ] **Step 3: Add the `IsSelf` field and update `tombstone()`**

In `backend/internal/contacts/contacts.go`, find the `Contact` struct's bookkeeping header:

```go
type Contact struct {
	UID       string `json:"uid"`
	Rev       int64  `json:"rev"`
	Deleted   bool   `json:"deleted,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`

	FormattedName string           `json:"fn"`
```

Replace with:

```go
type Contact struct {
	UID       string `json:"uid"`
	Rev       int64  `json:"rev"`
	Deleted   bool   `json:"deleted,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`

	// IsSelf marks this as the caller's own contact card — the (at most
	// one, enforced by Store.SetSelf) contact whose fields
	// api.handlePGPQRKey includes in the PGP QR key-exchange response.
	IsSelf bool `json:"isSelf,omitempty"`

	FormattedName string           `json:"fn"`
```

Then find `tombstone()`'s clear-list and add `IsSelf` to it — a tombstoned contact no longer represents a real person, so it must stop being reported as "me":

```go
	c.CustomFields = nil
	c.Pronouns = ""
	c.MergedUIDs = nil
```

Replace with:

```go
	c.CustomFields = nil
	c.Pronouns = ""
	c.IsSelf = false
	c.MergedUIDs = nil
```

- [ ] **Step 4: Preserve `IsSelf` in `Upsert`, and add `SetSelf`/`GetSelf`**

In `backend/internal/contacts/store.go`, find the update branch inside `Upsert`:

```go
	for i, existing := range s.contacts {
		if existing.UID == c.UID {
			if c.CreatedAt == "" {
				c.CreatedAt = existing.CreatedAt
			}
			s.contacts[i] = c
			if err := s.persistLocked(); err != nil {
				return Contact{}, err
			}
			return c, nil
		}
	}
```

Replace with:

```go
	for i, existing := range s.contacts {
		if existing.UID == c.UID {
			if c.CreatedAt == "" {
				c.CreatedAt = existing.CreatedAt
			}
			c.IsSelf = existing.IsSelf
			s.contacts[i] = c
			if err := s.persistLocked(); err != nil {
				return Contact{}, err
			}
			return c, nil
		}
	}
```

Then add two new methods after `Delete` (right before the `ChangedSince` doc comment):

```go
// SetSelf marks (self=true) or unmarks (self=false) the contact at uid as
// the caller's own contact card — the one api.handlePGPQRKey includes in
// the PGP QR key-exchange response. Marking a contact clears the flag from
// whichever contact previously held it, enforcing at most one self-contact
// per store. Returns found=false if uid doesn't exist.
func (s *Store) SetSelf(uid string, self bool) (Contact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshFromDiskLocked(); err != nil {
		return Contact{}, false, err
	}

	idx := -1
	for i, c := range s.contacts {
		if c.UID == uid {
			idx = i
			break
		}
	}
	if idx == -1 {
		return Contact{}, false, nil
	}

	if self {
		for i := range s.contacts {
			if i != idx {
				s.contacts[i].IsSelf = false
			}
		}
	}
	s.seq++
	s.contacts[idx].IsSelf = self
	s.contacts[idx].Rev = s.seq
	s.contacts[idx].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.persistLocked(); err != nil {
		return Contact{}, false, err
	}
	return s.contacts[idx], true, nil
}

// GetSelf returns the caller's own contact card — the (at most one) live
// contact with IsSelf set — or ok=false if none is set.
func (s *Store) GetSelf() (Contact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshFromDiskLocked(); err != nil {
		return Contact{}, false
	}
	for _, c := range s.contacts {
		if c.IsSelf && !c.Deleted {
			return c, true
		}
	}
	return Contact{}, false
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd backend && go test ./internal/contacts/... -v`
Expected: PASS — all tests in the package, including the new ones and every pre-existing test in `dedupe_test.go`/`search_test.go`.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/contacts/contacts.go backend/internal/contacts/store.go backend/internal/contacts/self_test.go
git commit -m "contacts: add IsSelf flag for the user's own contact card"
```

---

### Task 2: `POST /api/contacts/{id}/self` endpoint

**Files:**
- Create: `backend/internal/api/contacts_self.go`
- Modify: `backend/internal/api/server.go` (route registration)
- Test: `backend/internal/api/contacts_self_test.go` (new file)

**Interfaces:**
- Consumes: `Store.SetSelf(uid string, self bool) (contacts.Contact, bool, error)` from Task 1; `s.contactsFor(r *http.Request) (*contacts.Store, error)` (existing helper, `backend/internal/api/server_userscope.go:108`); `writeJSON(w http.ResponseWriter, status int, v any)` (existing helper, `backend/internal/api/server.go:3304`).
- Produces: `func (s *Server) handleContactSelf(w http.ResponseWriter, r *http.Request)`, registered at `POST /api/contacts/{id}/self`. Task 4 (frontend) calls this route by URL, not by Go symbol.

- [ ] **Step 1: Write the failing tests**

Create `backend/internal/api/contacts_self_test.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llama-lab/backend/internal/contacts"
)

func TestHandleContactSelf_SetsFlagAndReturnsUpdatedContact(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store, err := srv.userContactsStore(userID)
	if err != nil {
		t.Fatalf("userContactsStore: %v", err)
	}
	a, err := store.Upsert(contacts.Contact{FormattedName: "Alice"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	body, _ := json.Marshal(map[string]bool{"self": true})
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+a.UID+"/self", bytes.NewReader(body))
	req.SetPathValue("id", a.UID)
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleContactSelf)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var updated contacts.Contact
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if !updated.IsSelf {
		t.Fatalf("expected isSelf true in response, got %+v", updated)
	}

	self, ok := store.GetSelf()
	if !ok || self.UID != a.UID {
		t.Fatalf("GetSelf: ok=%v uid=%q, want %q", ok, self.UID, a.UID)
	}
}

func TestHandleContactSelf_UnknownIDReturns404(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(map[string]bool{"self": true})
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/nope/self", bytes.NewReader(body))
	req.SetPathValue("id", "nope")
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleContactSelf)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestContactSelfEndpointRoutesThroughRealMux drives the endpoint through
// the server's real route table (not a hand-wired middleware call) so it
// fails if the route registration in server.go is ever dropped — same
// pattern as TestContactsDedupeAcceptsSubscriberHash.
func TestContactSelfEndpointRoutesThroughRealMux(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store, err := srv.userContactsStore(userID)
	if err != nil {
		t.Fatalf("userContactsStore: %v", err)
	}
	a, err := store.Upsert(contacts.Contact{FormattedName: "Alice"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	body, _ := json.Marshal(map[string]bool{"self": true})
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+a.UID+"/self", bytes.NewReader(body))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (route should reach the handler); body=%s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd backend && go test ./internal/api/... -run TestHandleContactSelf -v` and `go test ./internal/api/... -run TestContactSelfEndpointRoutesThroughRealMux -v`
Expected: build failure — `srv.handleContactSelf undefined`.

- [ ] **Step 3: Implement the handler**

Create `backend/internal/api/contacts_self.go`:

```go
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// contactSelfPayload is the request body for POST /api/contacts/{id}/self.
type contactSelfPayload struct {
	Self bool `json:"self"`
}

// handleContactSelf marks or unmarks a contact as the caller's own contact
// card (contacts.Contact.IsSelf) — the one handlePGPQRKey includes in the
// PGP QR key-exchange response. At most one contact can hold the flag;
// store.SetSelf clears any previous holder.
func (s *Server) handleContactSelf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, err := s.contactsFor(r)
	if err != nil {
		http.Error(w, "failed to open contacts store", http.StatusInternalServerError)
		return
	}
	uid := strings.TrimSpace(r.PathValue("id"))
	if uid == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	var payload contactSelfPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&payload); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	updated, found, err := store.SetSelf(uid, payload.Self)
	if err != nil {
		http.Error(w, "failed to update contact", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
```

- [ ] **Step 4: Register the route**

In `backend/internal/api/server.go`, find:

```go
	mux.HandleFunc("POST /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
	mux.HandleFunc("GET /api/contacts/{id}/photo", s.withMailAuth(s.handleContactPhoto))
	mux.HandleFunc("DELETE /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
```

Replace with:

```go
	mux.HandleFunc("POST /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
	mux.HandleFunc("GET /api/contacts/{id}/photo", s.withMailAuth(s.handleContactPhoto))
	mux.HandleFunc("DELETE /api/contacts/{id}/photo", s.withAuth(s.handleContactPhoto))
	mux.HandleFunc("POST /api/contacts/{id}/self", s.withAuth(s.handleContactSelf))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd backend && go test ./internal/api/... -run 'TestHandleContactSelf|TestContactSelfEndpointRoutesThroughRealMux' -v`
Expected: PASS.

Run: `cd backend && go build -buildvcs=false ./... && go vet ./...`
Expected: no output, exit code 0.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/api/contacts_self.go backend/internal/api/contacts_self_test.go backend/internal/api/server.go
git commit -m "api: add POST /api/contacts/{id}/self to toggle the self-contact flag"
```

---

### Task 3: Include the contact card in the PGP QR key response

**Files:**
- Modify: `backend/internal/api/pgp_qr_handlers.go`
- Test: `backend/internal/api/pgp_qr_test.go` (append)

**Interfaces:**
- Consumes: `Store.GetSelf() (contacts.Contact, bool)` from Task 1; `s.userContactsStore(userID string) (*contacts.Store, error)` (existing helper, `backend/internal/api/server_userscope.go:97`).
- Produces: `type pgpQRContactCard struct{...}` and `func contactCardFromContact(c contacts.Contact) pgpQRContactCard` in `pgp_qr_handlers.go`; the `handlePGPQRKey` JSON response gains an optional `contactCard` field of that shape. Nothing later in this plan consumes these by name (the mobile-side consumer is out of scope), but keep the type exported-within-package name exactly `pgpQRContactCard` since the test in this task decodes it by name.

- [ ] **Step 1: Write the failing tests**

Append to `backend/internal/api/pgp_qr_test.go` (add `"llama-lab/backend/internal/contacts"` to the existing import block first):

```go
func TestPGPQRKeyIncludesContactCardWhenSelfContactSet(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)

	id, err := pgpmail.GenerateIdentity("Card Test", "card-test@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sealed, err := id.SealPrivateKey(srv.pgpPrivateKeyPath)
	if err != nil {
		t.Fatalf("SealPrivateKey: %v", err)
	}
	if _, err := srv.users.SetPGPIdentity(userID, id.Fingerprint, id.KeyID, id.ArmoredPublicKey, sealed, "generated", "2026-07-14T00:00:00Z"); err != nil {
		t.Fatalf("SetPGPIdentity: %v", err)
	}

	store, err := srv.userContactsStore(userID)
	if err != nil {
		t.Fatalf("userContactsStore: %v", err)
	}
	self, err := store.Upsert(contacts.Contact{
		FormattedName: "Jane Doe",
		Org:           "Acme",
		Emails:        []contacts.ContactValue{{Label: "work", Value: "jane@acme.example"}},
		PhotoRef:      "should-not-leak.jpg",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, err := store.SetSelf(self.UID, true); err != nil {
		t.Fatalf("SetSelf: %v", err)
	}

	token, _, err := srv.createPairingToken(userID, 2*time.Minute)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/pgp/qr/key?t="+token, nil)
	rec := httptest.NewRecorder()
	srv.handlePGPQRKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ContactCard *pgpQRContactCard `json:"contactCard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if resp.ContactCard == nil {
		t.Fatalf("expected contactCard in response, body=%s", rec.Body.String())
	}
	if resp.ContactCard.FormattedName != "Jane Doe" || resp.ContactCard.Org != "Acme" {
		t.Fatalf("contactCard = %+v, want fn=Jane Doe org=Acme", resp.ContactCard)
	}
	if len(resp.ContactCard.Emails) != 1 || resp.ContactCard.Emails[0].Value != "jane@acme.example" {
		t.Fatalf("contactCard emails = %+v", resp.ContactCard.Emails)
	}
	if strings.Contains(rec.Body.String(), "should-not-leak.jpg") {
		t.Fatalf("photoRef leaked into contactCard response: %s", rec.Body.String())
	}
}

func TestPGPQRKeyOmitsContactCardWhenNoSelfContact(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)

	id, err := pgpmail.GenerateIdentity("No Card Test", "no-card-test@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sealed, err := id.SealPrivateKey(srv.pgpPrivateKeyPath)
	if err != nil {
		t.Fatalf("SealPrivateKey: %v", err)
	}
	if _, err := srv.users.SetPGPIdentity(userID, id.Fingerprint, id.KeyID, id.ArmoredPublicKey, sealed, "generated", "2026-07-14T00:00:00Z"); err != nil {
		t.Fatalf("SetPGPIdentity: %v", err)
	}

	token, _, err := srv.createPairingToken(userID, 2*time.Minute)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/pgp/qr/key?t="+token, nil)
	rec := httptest.NewRecorder()
	srv.handlePGPQRKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "contactCard") {
		t.Fatalf("expected no contactCard field when no self-contact is set, body=%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd backend && go test ./internal/api/... -run 'TestPGPQRKeyIncludesContactCardWhenSelfContactSet|TestPGPQRKeyOmitsContactCardWhenNoSelfContact' -v`
Expected: build failure — `pgpQRContactCard undefined` (and, until the import is added, `contacts` package unused-or-undefined in the test file).

- [ ] **Step 3: Implement `pgpQRContactCard` and wire it into the handler**

In `backend/internal/api/pgp_qr_handlers.go`, change the imports from:

```go
import (
	"net/http"
	"strings"
	"time"
)
```

to:

```go
import (
	"net/http"
	"strings"
	"time"

	"llama-lab/backend/internal/contacts"
)
```

Then find the end of `handlePGPQRKey`:

```go
	u, err := s.users.Get(userID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        u.Username,
		"fingerprint": u.PGPFingerprint,
		"publicKey":   u.PGPPublicKey,
	})
}
```

Replace with:

```go
	u, err := s.users.Get(userID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusNotFound)
		return
	}
	resp := map[string]any{
		"name":        u.Username,
		"fingerprint": u.PGPFingerprint,
		"publicKey":   u.PGPPublicKey,
	}
	if store, err := s.userContactsStore(userID); err == nil {
		if self, ok := store.GetSelf(); ok {
			resp["contactCard"] = contactCardFromContact(self)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// pgpQRContactCard is the shareable subset of contacts.Contact included in
// the QR key-exchange response when the token owner has flagged a contact
// as their own (contacts.Contact.IsSelf). photoRef is deliberately excluded
// — contact photos are served from an authenticated route
// (GET /api/contacts/{id}/photo) that the scanning device, which
// authenticates with nothing but this token, has no session for. Bookkeeping
// and identity fields (uid, rev, isSelf, the contact's own pgpKey sub-field,
// merge markers) are excluded too: none are meaningful to a scanner, and the
// real PGP identity already rides this response's top-level
// fingerprint/publicKey, not whatever the contact's own pgpKey field holds.
type pgpQRContactCard struct {
	FormattedName      string                        `json:"fn,omitempty"`
	GivenName          string                        `json:"givenName,omitempty"`
	FamilyName         string                        `json:"familyName,omitempty"`
	MiddleName         string                        `json:"middleName,omitempty"`
	Prefix             string                        `json:"prefix,omitempty"`
	Suffix             string                        `json:"suffix,omitempty"`
	Nickname           string                        `json:"nickname,omitempty"`
	Org                string                        `json:"org,omitempty"`
	Title              string                        `json:"title,omitempty"`
	Emails             []contacts.ContactValue       `json:"emails,omitempty"`
	Phones             []contacts.ContactValue       `json:"phones,omitempty"`
	Addresses          []contacts.ContactAddress     `json:"addresses,omitempty"`
	Notes              string                        `json:"notes,omitempty"`
	Birthday           string                        `json:"birthday,omitempty"`
	IMs                []contacts.ContactIM          `json:"ims,omitempty"`
	Websites           []contacts.ContactURL         `json:"websites,omitempty"`
	Relations          []contacts.ContactRelation    `json:"relations,omitempty"`
	Events             []contacts.ContactEvent       `json:"events,omitempty"`
	PhoneticGivenName  string                        `json:"phoneticGivenName,omitempty"`
	PhoneticFamilyName string                        `json:"phoneticFamilyName,omitempty"`
	Department         string                        `json:"department,omitempty"`
	CustomFields       []contacts.ContactCustomField `json:"customFields,omitempty"`
	Pronouns           string                        `json:"pronouns,omitempty"`
}

func contactCardFromContact(c contacts.Contact) pgpQRContactCard {
	return pgpQRContactCard{
		FormattedName:      c.FormattedName,
		GivenName:          c.GivenName,
		FamilyName:         c.FamilyName,
		MiddleName:         c.MiddleName,
		Prefix:             c.Prefix,
		Suffix:             c.Suffix,
		Nickname:           c.Nickname,
		Org:                c.Org,
		Title:              c.Title,
		Emails:             c.Emails,
		Phones:             c.Phones,
		Addresses:          c.Addresses,
		Notes:              c.Notes,
		Birthday:           c.Birthday,
		IMs:                c.IMs,
		Websites:           c.Websites,
		Relations:          c.Relations,
		Events:             c.Events,
		PhoneticGivenName:  c.PhoneticGivenName,
		PhoneticFamilyName: c.PhoneticFamilyName,
		Department:         c.Department,
		CustomFields:       c.CustomFields,
		Pronouns:           c.Pronouns,
	}
}
```

Also add `"llama-lab/backend/internal/contacts"` to the import block at the top of `backend/internal/api/pgp_qr_test.go` (alongside the existing `"llama-lab/backend/internal/pgpmail"` import).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd backend && go test ./internal/api/... -run 'TestPGPQR' -v`
Expected: PASS for every `TestPGPQR*` test, including the pre-existing ones (`TestPGPQRTokenAndKeyRoundTrip`, `TestPGPQRKeyRejectsExpiredToken`, `TestPGPQREndpointsFailClosedOnUnsetPairingSecret`, `TestPGPQRTokenAcceptsSubscriberHash`, `TestPGPQRKeyRejectsTamperedSignature`) plus the two new ones.

Run: `cd backend && go build -buildvcs=false ./... && go vet ./... && go test ./...`
Expected: no errors; full suite passes.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/api/pgp_qr_handlers.go backend/internal/api/pgp_qr_test.go
git commit -m "api: include the user's contact card in the PGP QR key response"
```

---

### Task 4: Contacts page — mark a contact as "my card"

**Files:**
- Modify: `frontend/src/api/contacts.ts`
- Modify: `frontend/src/pages/ContactsPage.tsx`

**Interfaces:**
- Consumes: `POST /api/contacts/{id}/self` from Task 2 (`{"self": boolean}` → returns the updated `Contact` JSON, whose shape now includes `isSelf`).
- Produces: `isSelf?: boolean` added to the exported `Contact` type in `frontend/src/api/contacts.ts`; `setContactAsSelf(uid: string, self: boolean): Promise<Contact>` exported from the same file. Task 5 (`SecurityPage.tsx`) relies on `Contact.isSelf` existing on records returned by `listContacts()`.

- [ ] **Step 1: Add `isSelf` to the `Contact` type and a `setContactAsSelf` client function**

In `frontend/src/api/contacts.ts`, find:

```ts
export type Contact = {
  uid: string;
  rev: number;
  deleted?: boolean;
  createdAt: string;
  updatedAt: string;
  fn: string;
  givenName?: string;
  familyName?: string;
  middleName?: string;
  prefix?: string;
  suffix?: string;
  nickname?: string;
  org?: string;
  title?: string;
  emails?: ContactValue[];
  phones?: ContactValue[];
  addresses?: ContactAddress[];
  notes?: string;
  birthday?: string;
  mergedUIDs?: string[];
  mergedInto?: string;
} & ContactExtendedFields;
```

Replace with:

```ts
export type Contact = {
  uid: string;
  rev: number;
  deleted?: boolean;
  createdAt: string;
  updatedAt: string;
  fn: string;
  givenName?: string;
  familyName?: string;
  middleName?: string;
  prefix?: string;
  suffix?: string;
  nickname?: string;
  org?: string;
  title?: string;
  emails?: ContactValue[];
  phones?: ContactValue[];
  addresses?: ContactAddress[];
  notes?: string;
  birthday?: string;
  mergedUIDs?: string[];
  mergedInto?: string;
  isSelf?: boolean;
} & ContactExtendedFields;
```

Then find:

```ts
export function deleteContact(uid: string): Promise<{ ok: boolean; removed: boolean }> {
  return deleteJSON<{ ok: boolean; removed: boolean }>(`/api/contacts/${encodeURIComponent(uid)}`);
}
```

Replace with:

```ts
export function deleteContact(uid: string): Promise<{ ok: boolean; removed: boolean }> {
  return deleteJSON<{ ok: boolean; removed: boolean }>(`/api/contacts/${encodeURIComponent(uid)}`);
}

export function setContactAsSelf(uid: string, self: boolean): Promise<Contact> {
  return postJSON<Contact>(`/api/contacts/${encodeURIComponent(uid)}/self`, { self });
}
```

- [ ] **Step 2: Wire the toggle and badges into `ContactsPage.tsx`**

Add `setContactAsSelf` to the import block. Find:

```ts
import {
  bulkDeleteContacts,
  contactPhotoUrl,
  createContact,
  dedupeContacts,
  deleteContact,
  deleteContactPhoto,
  exportContactsUrl,
  importContacts,
  listContacts,
  updateContact,
  uploadContactPhoto,
```

Replace with:

```ts
import {
  bulkDeleteContacts,
  contactPhotoUrl,
  createContact,
  dedupeContacts,
  deleteContact,
  deleteContactPhoto,
  exportContactsUrl,
  importContacts,
  listContacts,
  setContactAsSelf,
  updateContact,
  uploadContactPhoto,
```

Add a `toggleSelfContact` function next to the existing `removeContact` function. Find:

```tsx
  async function removeContact(contact: Contact) {
    if (!window.confirm(`Delete ${contact.fn}?`)) {
      return;
    }
    setBusyId(contact.uid);
    setStatus("");
    try {
      await deleteContact(contact.uid);
      setStatus(`${contact.fn} deleted.`);
      if (editingUid === contact.uid) {
        closeForm();
      }
      if (selectedContact?.uid === contact.uid) {
        setSelectedContact(null);
      }
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to delete contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusyId("");
    }
  }
```

Replace with:

```tsx
  async function removeContact(contact: Contact) {
    if (!window.confirm(`Delete ${contact.fn}?`)) {
      return;
    }
    setBusyId(contact.uid);
    setStatus("");
    try {
      await deleteContact(contact.uid);
      setStatus(`${contact.fn} deleted.`);
      if (editingUid === contact.uid) {
        closeForm();
      }
      if (selectedContact?.uid === contact.uid) {
        setSelectedContact(null);
      }
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to delete contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusyId("");
    }
  }

  async function toggleSelfContact(contact: Contact) {
    setBusyId(contact.uid);
    setStatus("");
    try {
      const updated = await setContactAsSelf(contact.uid, !contact.isSelf);
      setStatus(updated.isSelf ? `${updated.fn} set as your contact card.` : `${updated.fn} removed as your contact card.`);
      const next = await loadContacts();
      if (selectedContact?.uid === updated.uid) {
        setSelectedContact(next.find((c) => c.uid === updated.uid) ?? null);
      }
    } catch (error: unknown) {
      setStatus(`Failed to update contact card: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusyId("");
    }
  }
```

Add a small badge to the list row. Find:

```tsx
                              <div className="contacts-identity-text">
                                <span className="contacts-name">{contact.fn}</span>
                                {contact.org ? <span className="contacts-sub">{contact.org}</span> : null}
                              </div>
```

Replace with:

```tsx
                              <div className="contacts-identity-text">
                                <span className="contacts-name">{contact.fn}</span>
                                {contact.isSelf ? <span className="contacts-sub">Your contact card</span> : null}
                                {contact.org ? <span className="contacts-sub">{contact.org}</span> : null}
                              </div>
```

Add the toggle button and a badge to the contact detail panel. Find:

```tsx
                  {selectedContact.org || selectedContact.title ? (
                    <p className="contacts-sub" style={{ margin: "2px 0 0" }}>
                      {[selectedContact.title, selectedContact.org, selectedContact.department].filter(Boolean).join(" · ")}
                    </p>
                  ) : null}
                </div>
              </div>
              <div className="contact-details-actions">
                <button
                  type="button"
                  onClick={() => {
                    setSelectedContact(null);
                    openEditForm(selectedContact);
                  }}
                >
                  Edit
                </button>
```

Replace with:

```tsx
                  {selectedContact.org || selectedContact.title ? (
                    <p className="contacts-sub" style={{ margin: "2px 0 0" }}>
                      {[selectedContact.title, selectedContact.org, selectedContact.department].filter(Boolean).join(" · ")}
                    </p>
                  ) : null}
                  {selectedContact.isSelf ? (
                    <p className="contacts-sub" style={{ margin: "2px 0 0" }}>
                      Your contact card — shared when someone scans your PGP QR code
                    </p>
                  ) : null}
                </div>
              </div>
              <div className="contact-details-actions">
                <button
                  type="button"
                  onClick={() => void toggleSelfContact(selectedContact)}
                  disabled={busyId === selectedContact.uid}
                >
                  {selectedContact.isSelf ? "Remove as my card" : "Use as my card"}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setSelectedContact(null);
                    openEditForm(selectedContact);
                  }}
                >
                  Edit
                </button>
```

- [ ] **Step 3: Verify the build**

Run: `cd frontend && npm run build`
Expected: builds with zero TypeScript errors.

- [ ] **Step 4: Manual verification**

Run `cd frontend && npm run dev` and, against a running backend, open the Contacts page:
1. Select a contact, click "Use as my card" — the button flips to "Remove as my card", a "Your contact card" line appears under the name, and the same label appears on that row in the list.
2. Select a second contact and click "Use as my card" on it — confirm the first contact's badge disappears (open it again to check) and only the second carries the badge, matching the backend's enforced uniqueness.
3. Click "Remove as my card" — badge disappears everywhere.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/api/contacts.ts frontend/src/pages/ContactsPage.tsx
git commit -m "frontend: add a way to mark a contact as the user's own card"
```

---

### Task 5: Security page — surface the contact-card status

**Files:**
- Modify: `frontend/src/pages/SecurityPage.tsx`

**Interfaces:**
- Consumes: `listContacts(): Promise<Contact[]>` and `type Contact` (with `isSelf`) from `frontend/src/api/contacts.ts` (Task 4); `Link` from `react-router-dom`.
- Produces: nothing consumed elsewhere — this is the last task.

- [ ] **Step 1: Add imports and state for the self-contact**

Find:

```tsx
import { FormEvent, useEffect, useState } from "react";
import QRCode from "qrcode";
import { getJSON, postJSON, putJSON, toErrorMessage } from "../api/client";
import { getPGPIdentity, generatePGPIdentity, importPGPIdentity, deletePGPIdentity, type PGPIdentity } from "../api/pgp";
```

Replace with:

```tsx
import { FormEvent, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import QRCode from "qrcode";
import { getJSON, postJSON, putJSON, toErrorMessage } from "../api/client";
import { getPGPIdentity, generatePGPIdentity, importPGPIdentity, deletePGPIdentity, type PGPIdentity } from "../api/pgp";
import { listContacts, type Contact } from "../api/contacts";
```

Find the PGP identity state block:

```tsx
  // PGP identity state.
  const [pgpIdentity, setPgpIdentity] = useState<PGPIdentity | null>(null);
  const [pgpLoading, setPgpLoading] = useState(true);
  const [pgpBusy, setPgpBusy] = useState(false);
  const [pgpStatus, setPgpStatus] = useState("");
  const [pgpImportOpen, setPgpImportOpen] = useState(false);
  const [pgpImportKey, setPgpImportKey] = useState("");
  const [pgpImportPassphrase, setPgpImportPassphrase] = useState("");
```

Replace with:

```tsx
  // PGP identity state.
  const [pgpIdentity, setPgpIdentity] = useState<PGPIdentity | null>(null);
  const [pgpLoading, setPgpLoading] = useState(true);
  const [pgpBusy, setPgpBusy] = useState(false);
  const [pgpStatus, setPgpStatus] = useState("");
  const [pgpImportOpen, setPgpImportOpen] = useState(false);
  const [pgpImportKey, setPgpImportKey] = useState("");
  const [pgpImportPassphrase, setPgpImportPassphrase] = useState("");
  const [selfContact, setSelfContact] = useState<Contact | null>(null);
```

Find the PGP identity `useEffect`:

```tsx
  useEffect(() => {
    let cancelled = false;
    getPGPIdentity()
      .then((id) => {
        if (!cancelled) setPgpIdentity(id);
      })
      .catch(() => {
        if (!cancelled) setPgpIdentity(null);
      })
      .finally(() => {
        if (!cancelled) setPgpLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);
```

Replace with:

```tsx
  useEffect(() => {
    let cancelled = false;
    getPGPIdentity()
      .then((id) => {
        if (!cancelled) setPgpIdentity(id);
      })
      .catch(() => {
        if (!cancelled) setPgpIdentity(null);
      })
      .finally(() => {
        if (!cancelled) setPgpLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    listContacts()
      .then((all) => {
        if (!cancelled) setSelfContact(all.find((c) => c.isSelf) ?? null);
      })
      .catch(() => {
        if (!cancelled) setSelfContact(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);
```

- [ ] **Step 2: Add the status line to the PGP identity card**

Find:

```tsx
          {pgpLoading ? (
            <p className="contacts-muted">Loading...</p>
          ) : pgpIdentity ? (
            <>
              <p className="contacts-pgp-fingerprint">
                Fingerprint: {pgpIdentity.fingerprint} · Source: {pgpIdentity.source}
              </p>
              <details>
```

Replace with:

```tsx
          {pgpLoading ? (
            <p className="contacts-muted">Loading...</p>
          ) : pgpIdentity ? (
            <>
              <p className="contacts-pgp-fingerprint">
                Fingerprint: {pgpIdentity.fingerprint} · Source: {pgpIdentity.source}
              </p>
              <p className="contacts-muted">
                {selfContact ? (
                  <>Sharing contact card: {selfContact.fn} · <Link to="/contacts">Manage in Contacts</Link></>
                ) : (
                  <>No contact card set — <Link to="/contacts">add one in Contacts</Link> and mark it as yours to include it when sharing your PGP key.</>
                )}
              </p>
              <details>
```

- [ ] **Step 3: Verify the build**

Run: `cd frontend && npm run build`
Expected: builds with zero TypeScript errors.

- [ ] **Step 4: Manual verification**

With a running backend, and a PGP identity already configured (Security page → Email Encryption (PGP)):
1. With no contact flagged `isSelf`: the PGP card shows "No contact card set — add one in Contacts and mark it as yours...", and the link navigates to `/contacts`.
2. Go mark a contact as "my card" (Task 4's toggle), return to Security: the line now reads "Sharing contact card: `<name>`" with a working "Manage in Contacts" link.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/pages/SecurityPage.tsx
git commit -m "frontend: surface the shared contact-card status on the Security page"
```

---

## Self-Review Notes

- **Spec coverage:** Data model (Task 1) ✓, API self-toggle endpoint (Task 2) ✓, `contactCard` on `handlePGPQRKey` with photo/bookkeeping exclusions (Task 3) ✓, Contacts-page entry point (Task 4) ✓, Security-page surfacing (Task 5) ✓. Testing section of the spec is covered by Tasks 1–3's Go tests; the spec's note that neither frontend page has an existing test file is respected by Tasks 4–5 (build + manual verification only, no new test framework introduced).
- **Type consistency:** `Store.SetSelf(uid string, self bool) (Contact, bool, error)` and `Store.GetSelf() (Contact, bool)` (Task 1) are called with these exact signatures in Task 2 and Task 3. `pgpQRContactCard` (Task 3) is decoded by that exact name in Task 3's own tests; nothing outside Task 3 references it. `setContactAsSelf(uid: string, self: boolean): Promise<Contact>` (Task 4) matches its one call site in the same task. `Contact.isSelf` (Task 4) is read in Task 5 exactly as added.
- **Scope:** Single subsystem (this repo's backend + web frontend), matching the spec's explicit scope cut — no llama-mobile changes.
