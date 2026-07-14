package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	imapadapter "llama-lab/backend/internal/adapters/imap"
	"llama-lab/backend/internal/contacts"
	"llama-lab/backend/internal/groups"
	"llama-lab/backend/internal/mailcache"
	"llama-lab/backend/internal/state"
)

// errIMAPNotConfigured is returned when a caller has not stored IMAP
// credentials yet; handlers translate it into a 400 with a clear message.
var errIMAPNotConfigured = errors.New("imap configuration is required")

// errMailUnauthorized and errMailPairingNotConfigured are the two failure
// modes withMailAuth distinguishes: a bad/missing credential (401) versus a
// mobile-shaped request (sub+hash supplied) hitting a server that has no
// PAIRING_SECRET set (503) — mirroring handleContactsSync's precedent
// without misreporting 503 for an ordinary unauthenticated web request.
var (
	errMailUnauthorized         = errors.New("unauthorized")
	errMailPairingNotConfigured = errors.New("pairing is not configured")
)

func (s *Server) userConfigDir(userID string) string {
	return filepath.Join(s.configDir, "users", userID)
}

func (s *Server) userStateDir(userID string) string {
	return filepath.Join(s.stateDir, "users", userID)
}

func (s *Server) userIMAPConfigPath(userID string) string {
	return filepath.Join(s.userConfigDir(userID), "imap-config.json")
}

func (s *Server) userTuningPath(userID string) string {
	return filepath.Join(s.userConfigDir(userID), "tuning.md")
}

func (s *Server) userSettingsPath(userID string) string {
	return filepath.Join(s.userConfigDir(userID), "config.yaml")
}

// userCardDAVAuthPath is where the user's app-specific CardDAV password hash
// is stored — separate from imap-config.json since it's a plain scrypt hash
// (not reversible credentials), so it needs no encryption-at-rest key.
func (s *Server) userCardDAVAuthPath(userID string) string {
	return filepath.Join(s.userConfigDir(userID), "carddav-auth.json")
}

// userCardDAVClientConfigPath is where the user's outbound CardDAV client
// credentials (for pulling contacts from an external CardDAV server) are
// stored, encrypted at rest the same way as imap-config.json.
func (s *Server) userCardDAVClientConfigPath(userID string) string {
	return filepath.Join(s.userConfigDir(userID), "carddav-client.json")
}

func (s *Server) userStore(userID string) (*state.Store, error) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if st, ok := s.userStores[userID]; ok {
		return st, nil
	}
	st, err := state.New(s.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	s.userStores[userID] = st
	return st, nil
}

// storeFor resolves the calling user's state store from the request's
// AuthContext (requires the handler to be wrapped in withAuth).
func (s *Server) storeFor(r *http.Request) (*state.Store, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.userStore(ac.UserID)
}

func (s *Server) userContactsStore(userID string) (*contacts.Store, error) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if st, ok := s.userContacts[userID]; ok {
		return st, nil
	}
	st, err := contacts.New(s.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	s.userContacts[userID] = st
	return st, nil
}

// contactsFor resolves the calling user's contacts store from the request's
// AuthContext (requires the handler to be wrapped in withAuth or withMailAuth,
// or otherwise inject an AuthContext).
func (s *Server) contactsFor(r *http.Request) (*contacts.Store, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.userContactsStore(ac.UserID)
}

func (s *Server) userGroupsStore(userID string) (*groups.Store, error) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if st, ok := s.userGroups[userID]; ok {
		return st, nil
	}
	st, err := groups.New(s.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	s.userGroups[userID] = st
	return st, nil
}

// groupsFor resolves the calling user's groups store from the request's
// AuthContext (requires the handler to be wrapped in withAuth).
func (s *Server) groupsFor(r *http.Request) (*groups.Store, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.userGroupsStore(ac.UserID)
}

// sanitizeGroupIDsForUser drops any group ID that isn't a real group owned
// by userID, so a stale or forged ID from a client can't create a dangling
// Contact.GroupIDs reference.
func (s *Server) sanitizeGroupIDsForUser(userID string, ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	gs, err := s.userGroupsStore(userID)
	if err != nil {
		return nil
	}
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := gs.Get(id); ok {
			kept = append(kept, id)
		}
	}
	return kept
}

// userContactPhotosDir is where a user's contact photo files live, one
// content-hashed file per uploaded photo.
func (s *Server) userContactPhotosDir(userID string) string {
	return filepath.Join(s.userStateDir(userID), "contact-photos")
}

// userContactPhotoPath resolves a photoRef to its on-disk path.
// filepath.Base guards against path traversal from a hostile ref.
func (s *Server) userContactPhotoPath(userID, ref string) string {
	return filepath.Join(s.userContactPhotosDir(userID), filepath.Base(ref))
}

func (s *Server) userMailCacheStore(userID string) (*mailcache.Store, error) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if st, ok := s.userMailCache[userID]; ok {
		return st, nil
	}
	st, err := mailcache.New(s.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	s.userMailCache[userID] = st
	return st, nil
}

// mailCacheFor resolves the calling user's mail cache store from the
// request's AuthContext (requires the handler to be wrapped in
// withMailAuth, as handleInbox already is).
func (s *Server) mailCacheFor(r *http.Request) (*mailcache.Store, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.userMailCacheStore(ac.UserID)
}

type serverMailEntry struct {
	client    imapadapter.Client
	updatedAt string
}

// userMailClient returns a cached IMAP client for the user, rebuilt whenever
// their stored credential payload changes (keyed by the payload UpdatedAt).
// Returns errIMAPNotConfigured when the user has no stored credentials.
func (s *Server) userMailClient(userID string) (imapadapter.Client, error) {
	payload, exists, err := readIMAPConfigPayload(s.userIMAPConfigPath(userID), s.imapConfigKeyPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errIMAPNotConfigured
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if entry, ok := s.userMail[userID]; ok && entry.updatedAt == payload.UpdatedAt {
		return entry.client, nil
	}
	client := imapadapter.NewAPIClientFromStoredConfig(s.userIMAPConfigPath(userID), s.imapConfigKeyPath)
	s.userMail[userID] = &serverMailEntry{client: client, updatedAt: payload.UpdatedAt}
	return client, nil
}

func (s *Server) mailFor(r *http.Request) (imapadapter.Client, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.userMailClient(ac.UserID)
}

// resolveMailAuthContext authenticates a mail request either by session
// cookie (web) or by subscriberId/subscriberHash query params (mobile,
// reusing the same pairing trust boundary as native push and contacts
// sync — see contacts_handlers.go's handleContactsSync). Mobile never sees
// or sets raw IMAP/SMTP credentials; it only acts on an account already
// configured through the web UI.
func (s *Server) resolveMailAuthContext(r *http.Request) (AuthContext, error) {
	if ac, ok := s.currentUser(r); ok {
		return ac, nil
	}
	subscriberID := strings.TrimSpace(r.URL.Query().Get("sub"))
	subscriberHash := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hash")))
	if subscriberID == "" || subscriberHash == "" {
		return AuthContext{}, errMailUnauthorized
	}
	if s.pairingSecret == "" {
		return AuthContext{}, errMailPairingNotConfigured
	}
	expectedHash := s.pairingSubscriberHash(subscriberID)
	if subtle.ConstantTimeCompare([]byte(subscriberHash), []byte(expectedHash)) != 1 {
		return AuthContext{}, errMailUnauthorized
	}
	ownerID, ok := s.lookupUserBySubscriber(subscriberID)
	if !ok {
		return AuthContext{}, errMailUnauthorized
	}
	return AuthContext{UserID: ownerID}, nil
}

func (s *Server) invalidateUserMail(userID string) {
	s.userMu.Lock()
	delete(s.userMail, userID)
	s.userMu.Unlock()
}

// lookupUserBySubscriber maps a per-user subscriber ID back to its owning
// user, for the unauthenticated native-register endpoint. The in-memory
// index is lazily rebuilt on a miss so a subscriber ID minted after server
// start is still found without a restart.
func (s *Server) lookupUserBySubscriber(subscriberID string) (string, bool) {
	subscriberID = strings.TrimSpace(subscriberID)
	if subscriberID == "" {
		return "", false
	}
	s.userMu.Lock()
	if userID, ok := s.subIndex[subscriberID]; ok {
		s.userMu.Unlock()
		return userID, true
	}
	s.userMu.Unlock()

	s.rescanSubscriberIndex()

	s.userMu.Lock()
	defer s.userMu.Unlock()
	userID, ok := s.subIndex[subscriberID]
	return userID, ok
}

// rescanSubscriberIndex rebuilds subscriberID -> userID by reading every
// per-user state.json. Cheap at this scale (a handful of small files).
func (s *Server) rescanSubscriberIndex() {
	usersDir := filepath.Join(s.stateDir, "users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return
	}
	next := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(usersDir, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var doc struct {
			SubscriberID string `json:"subscriberId"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		if id := strings.TrimSpace(doc.SubscriberID); id != "" {
			next[id] = e.Name()
		}
	}
	s.userMu.Lock()
	s.subIndex = next
	s.userMu.Unlock()
}
