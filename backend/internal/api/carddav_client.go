package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"llama-lab/backend/internal/contacts"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

// carddavClientConfigPayload is the caller's outbound CardDAV client
// configuration: credentials for an external CardDAV server (e.g. iCloud,
// Nextcloud, Google) that this account should pull contacts from. It is
// encrypted at rest the same way as imapConfigPayload. AddressBookPath is
// discovered once (principal -> home set -> address book) and cached so
// later syncs skip the discovery round trips; the sync-status fields
// (LastSynced*) are informational and rewritten after every sync attempt.
type carddavClientConfigPayload struct {
	ServerURL       string `json:"serverUrl"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	AddressBookPath string `json:"addressBookPath,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`

	LastSyncedAt     string `json:"lastSyncedAt,omitempty"`
	LastSyncError    string `json:"lastSyncError,omitempty"`
	LastSyncImported int    `json:"lastSyncImported,omitempty"`
	LastSyncUpdated  int    `json:"lastSyncUpdated,omitempty"`
}

func normalizeCardDAVClientPayload(p carddavClientConfigPayload) carddavClientConfigPayload {
	p.ServerURL = strings.TrimSpace(p.ServerURL)
	p.Username = strings.TrimSpace(p.Username)
	p.Password = strings.TrimSpace(p.Password)
	p.AddressBookPath = strings.TrimSpace(p.AddressBookPath)
	return p
}

func readCardDAVClientConfigPayload(path, keyPath string) (carddavClientConfigPayload, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return carddavClientConfigPayload{}, false, nil
		}
		return carddavClientConfigPayload{}, false, err
	}
	plain, err := decryptEncryptedPayload(b, keyPath)
	if err != nil {
		return carddavClientConfigPayload{}, false, err
	}
	var payload carddavClientConfigPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return carddavClientConfigPayload{}, false, err
	}
	return normalizeCardDAVClientPayload(payload), true, nil
}

func writeCardDAVClientConfigPayload(path, keyPath string, payload carddavClientConfigPayload) error {
	plain, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeEncryptedPayload(path, keyPath, plain)
}

// handleContactsCardDAVClientConfig manages the caller's outbound CardDAV
// client credentials: GET reports the stored config (never the password),
// POST (re)saves it, DELETE removes it.
func (s *Server) handleContactsCardDAVClientConfig(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	cfgPath := s.userCardDAVClientConfigPath(ac.UserID)
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := readCardDAVClientConfigPayload(cfgPath, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read carddav client configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		writeJSON(w, http.StatusOK, cardDAVClientStatusResponse(payload))
	case http.MethodPost:
		var payload carddavClientConfigPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		payload = normalizeCardDAVClientPayload(payload)
		if payload.ServerURL == "" || payload.Username == "" || payload.Password == "" {
			http.Error(w, "serverUrl, username, and password are required", http.StatusBadRequest)
			return
		}
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		if err := writeCardDAVClientConfigPayload(cfgPath, s.imapConfigKeyPath, payload); err != nil {
			http.Error(w, "failed to save carddav client configuration", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, cardDAVClientStatusResponse(payload))
	case http.MethodDelete:
		if err := os.Remove(cfgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove carddav client configuration", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func cardDAVClientStatusResponse(payload carddavClientConfigPayload) map[string]any {
	return map[string]any{
		"configured":       true,
		"serverUrl":        payload.ServerURL,
		"username":         payload.Username,
		"addressBookPath":  payload.AddressBookPath,
		"updatedAt":        payload.UpdatedAt,
		"lastSyncedAt":     payload.LastSyncedAt,
		"lastSyncError":    payload.LastSyncError,
		"lastSyncImported": payload.LastSyncImported,
		"lastSyncUpdated":  payload.LastSyncUpdated,
	}
}

// handleContactsCardDAVClientSync pulls the caller's address book down from
// the configured external CardDAV server now, upserting every remote card
// into the caller's local contacts.Store. From there the existing mobile
// sync endpoint (handleContactsSync) and CardDAV server surface pick the
// imported contacts up exactly like any locally-created one — this handler
// only ever reads from the remote server, never writes back to it.
func (s *Server) handleContactsCardDAVClientSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	cfgPath := s.userCardDAVClientConfigPath(ac.UserID)
	payload, exists, err := readCardDAVClientConfigPayload(cfgPath, s.imapConfigKeyPath)
	if err != nil {
		http.Error(w, "failed to read carddav client configuration", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "carddav client is not configured", http.StatusBadRequest)
		return
	}
	store, err := s.userContactsStore(ac.UserID)
	if err != nil {
		http.Error(w, "failed to open contacts store", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	imported, updated, addressBookPath, syncErr := syncCardDAVClient(ctx, payload, store)

	payload.LastSyncedAt = time.Now().UTC().Format(time.RFC3339)
	if syncErr != nil {
		payload.LastSyncError = syncErr.Error()
	} else {
		payload.LastSyncError = ""
		payload.LastSyncImported = imported
		payload.LastSyncUpdated = updated
		if addressBookPath != "" {
			payload.AddressBookPath = addressBookPath
		}
	}
	// Best-effort: persist the sync outcome even if it failed, so the UI can
	// show the last error without needing a separate status endpoint.
	_ = writeCardDAVClientConfigPayload(cfgPath, s.imapConfigKeyPath, payload)

	if syncErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": syncErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"imported":        imported,
		"updated":         updated,
		"addressBookPath": addressBookPath,
		"syncedAt":        payload.LastSyncedAt,
	})
}

// syncCardDAVClient connects to the external CardDAV server described by cfg,
// discovers its address book (unless AddressBookPath is already cached from
// a previous sync), and upserts every card it finds into store.
func syncCardDAVClient(ctx context.Context, cfg carddavClientConfigPayload, store *contacts.Store) (imported, updated int, addressBookPath string, err error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	authed := webdav.HTTPClientWithBasicAuth(httpClient, cfg.Username, cfg.Password)
	client, err := carddav.NewClient(authed, cfg.ServerURL)
	if err != nil {
		return 0, 0, "", fmt.Errorf("connect to carddav server: %w", err)
	}

	addressBookPath = cfg.AddressBookPath
	if addressBookPath == "" {
		principal, err := client.FindCurrentUserPrincipal(ctx)
		if err != nil {
			return 0, 0, "", fmt.Errorf("discover current user principal: %w", err)
		}
		homeSet, err := client.FindAddressBookHomeSet(ctx, principal)
		if err != nil {
			return 0, 0, "", fmt.Errorf("discover address book home set: %w", err)
		}
		books, err := client.FindAddressBooks(ctx, homeSet)
		if err != nil {
			return 0, 0, "", fmt.Errorf("discover address books: %w", err)
		}
		if len(books) == 0 {
			return 0, 0, "", errors.New("no address books found on remote server")
		}
		addressBookPath = books[0].Path
	}

	objects, err := client.QueryAddressBook(ctx, addressBookPath, &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true},
	})
	if err != nil {
		return 0, 0, addressBookPath, fmt.Errorf("fetch address book contents: %w", err)
	}

	for _, obj := range objects {
		uid := remoteContactUID(obj)
		_, existed := store.Get(uid)
		if _, err := store.Upsert(contactFromVCard(uid, obj.Card)); err != nil {
			return imported, updated, addressBookPath, fmt.Errorf("save contact %s: %w", uid, err)
		}
		if existed {
			updated++
		} else {
			imported++
		}
	}
	return imported, updated, addressBookPath, nil
}

// remoteContactUID derives the local store UID for a card pulled from an
// external CardDAV server: the vCard's own UID when it has one, so re-syncing
// updates the same local contact instead of duplicating it, or a stable hash
// of its resource path otherwise. Namespaced so it can never collide with a
// UUID assigned to a locally-created contact.
func remoteContactUID(obj carddav.AddressObject) string {
	if uid := strings.TrimSpace(obj.Card.Value(vcard.FieldUID)); uid != "" {
		return "carddav-import-" + uid
	}
	sum := sha1.Sum([]byte(obj.Path))
	return "carddav-import-" + hex.EncodeToString(sum[:])
}
