package pgpmail

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kypost-server/backend/internal/cryptutil"
	"kypost-server/backend/internal/fsutil"
)

// PickupRecord is one queued message a recipient without a known PGP key
// can retrieve once via an authenticated link, in place of receiving PGP-
// encrypted content they have no key to read.
type PickupRecord struct {
	ID             string                     `json:"id"`
	SenderUserID   string                     `json:"senderUserId"`
	RecipientEmail string                     `json:"recipientEmail"`
	Subject        string                     `json:"subject"`
	BodyEnc        cryptutil.EncryptedPayload `json:"bodyEnc"`
	CreatedAt      string                     `json:"createdAt"`
	ExpiresAt      string                     `json:"expiresAt"`
	Viewed         bool                       `json:"viewed"`
}

// PickupStore is the global (not per-user — the recipient has no account)
// store of pending pickup-link messages, one file per record under baseDir.
type PickupStore struct {
	mu      sync.Mutex
	baseDir string
	keyPath string
}

// NewPickupStore opens the pickup store rooted at baseDir (typically
// $STATE_DIR/pickup), sealing bodies with the master key at keyPath.
func NewPickupStore(baseDir, keyPath string) *PickupStore {
	return &PickupStore{baseDir: baseDir, keyPath: keyPath}
}

func (s *PickupStore) recordPath(id string) string {
	return filepath.Join(s.baseDir, id+".json")
}

// Create seals body and persists a new pickup record, expiring after ttl.
// Returns the record's ID, used to build the pickup link.
func (s *PickupStore) Create(senderUserID, recipientEmail, subject, body string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := fsutil.NewUUIDv4()
	if err != nil {
		return "", err
	}
	key, err := cryptutil.LoadOrCreateKey(s.keyPath)
	if err != nil {
		return "", err
	}
	bodyEnc, err := cryptutil.Seal([]byte(body), key)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	record := PickupRecord{
		ID:             id,
		SenderUserID:   senderUserID,
		RecipientEmail: recipientEmail,
		Subject:        subject,
		BodyEnc:        bodyEnc,
		CreatedAt:      now.Format(time.RFC3339),
		ExpiresAt:      now.Add(ttl).Format(time.RFC3339),
	}
	if err := s.save(record); err != nil {
		return "", err
	}
	return id, nil
}

func (s *PickupStore) save(record PickupRecord) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(s.recordPath(record.ID), b, 0o600)
}

var ErrPickupNotFound = errors.New("pgpmail: pickup record not found")
var ErrPickupExpired = errors.New("pgpmail: pickup record expired or already viewed")

// View opens a pickup record's body exactly once: on success it returns the
// decrypted subject/body and immediately deletes the sealed body from disk,
// keeping a viewed tombstone so a repeat visit reports ErrPickupExpired
// instead of ErrPickupNotFound. Also returns ErrPickupExpired if the TTL has
// passed — "expire after N days or first view, whichever comes first."
func (s *PickupStore) View(id string) (subject, body string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.recordPath(id))
	if os.IsNotExist(err) {
		return "", "", ErrPickupNotFound
	}
	if err != nil {
		return "", "", err
	}
	var record PickupRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return "", "", err
	}

	if record.Viewed {
		return "", "", ErrPickupExpired
	}
	expiresAt, err := time.Parse(time.RFC3339, record.ExpiresAt)
	if err == nil && time.Now().UTC().After(expiresAt) {
		record.Viewed = true
		record.BodyEnc = cryptutil.EncryptedPayload{}
		_ = s.save(record)
		return "", "", ErrPickupExpired
	}

	key, err := cryptutil.LoadOrCreateKey(s.keyPath)
	if err != nil {
		return "", "", err
	}
	plain, err := cryptutil.Open(record.BodyEnc, key)
	if err != nil {
		return "", "", err
	}

	record.Viewed = true
	record.BodyEnc = cryptutil.EncryptedPayload{}
	if err := s.save(record); err != nil {
		return "", "", err
	}
	return record.Subject, string(plain), nil
}

// Sweep deletes tombstones (already-viewed or expired-and-unviewed records)
// older than retention, keeping the pickup directory from growing forever.
func (s *PickupStore) Sweep(retention time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.baseDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.baseDir, entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record PickupRecord
		if err := json.Unmarshal(b, &record); err != nil {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339, record.CreatedAt)
		if err != nil || createdAt.Before(cutoff) {
			_ = os.Remove(path)
		}
	}
	return nil
}
