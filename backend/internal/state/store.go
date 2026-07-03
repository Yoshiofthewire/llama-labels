package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu                 sync.Mutex
	baseDir            string
	checkpoint         string
	processedSet       map[string]time.Time
	decisions          []Decision
	notifications      []NotificationSubscription
	notificationsDirty bool
	subscriberID       string

	aiCreditsExhausted   bool
	aiCreditsExhaustedAt string
}

type Decision struct {
	MessageID string `json:"messageId"`
	Sender    string `json:"sender"`
	SentTo    string `json:"sentTo,omitempty"`
	Subject   string `json:"subject"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	AtUTC     string `json:"atUtc"`
}

type NotificationSubscription struct {
	Endpoint  string `json:"endpoint"`
	Auth      string `json:"auth"`
	P256DH    string `json:"p256dh"`
	UserAgent string `json:"userAgent,omitempty"`
	UpdatedAt string `json:"updatedAt"`
}

type stateFile struct {
	LastCheckpoint       string                     `json:"lastCheckpoint"`
	Processed            map[string]string          `json:"processed"`
	Notifications        []NotificationSubscription `json:"notifications,omitempty"`
	SubscriberID         string                     `json:"subscriberId,omitempty"`
	AICreditsExhausted   bool                       `json:"aiCreditsExhausted,omitempty"`
	AICreditsExhaustedAt string                     `json:"aiCreditsExhaustedAt,omitempty"`
}

func New(baseDir string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{baseDir: baseDir, processedSet: map[string]time.Time{}, decisions: []Decision{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.loadDecisions(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.baseDir, "state.json")
}

func (s *Store) decisionsPath() string {
	return filepath.Join(s.baseDir, "decisions.json")
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.persistLocked()
		}
		return err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	s.applyStateFile(sf)
	return nil
}

func (s *Store) applyStateFile(sf stateFile) {
	s.checkpoint = sf.LastCheckpoint
	s.aiCreditsExhausted = sf.AICreditsExhausted
	s.aiCreditsExhaustedAt = sf.AICreditsExhaustedAt
	s.subscriberID = strings.TrimSpace(sf.SubscriberID)
	s.notifications = append([]NotificationSubscription{}, sf.Notifications...)
	s.notificationsDirty = false

	processed := make(map[string]time.Time, len(sf.Processed))
	for id, ts := range sf.Processed {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		processed[id] = t
	}
	s.processedSet = processed
}

func (s *Store) refreshStateFromDiskLocked() error {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	s.applyStateFile(sf)
	return nil
}

func (s *Store) refreshNotificationsFromDiskLocked() error {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var sf struct {
		Notifications []NotificationSubscription `json:"notifications,omitempty"`
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	s.notifications = append([]NotificationSubscription{}, sf.Notifications...)
	s.notificationsDirty = false
	return nil
}

func (s *Store) Cleanup(keepDays int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
	for id, ts := range s.processedSet {
		if ts.Before(cutoff) {
			delete(s.processedSet, id)
		}
	}
	trimmed := make([]Decision, 0, len(s.decisions))
	for _, d := range s.decisions {
		t, err := time.Parse(time.RFC3339, d.AtUTC)
		if err != nil || !t.Before(cutoff) {
			trimmed = append(trimmed, d)
		}
	}
	s.decisions = trimmed
	if err := s.persistDecisionsLocked(); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.processedSet[id]
	return ok
}

func (s *Store) MarkProcessed(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processedSet[id] = time.Now().UTC()
	return s.persistLocked()
}

func (s *Store) SetCheckpoint(value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint = value
	return s.persistLocked()
}

func (s *Store) Checkpoint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint
}

func (s *Store) GetOrCreateSubscriberID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.refreshStateFromDiskLocked(); err != nil {
		return "", err
	}
	if s.subscriberID != "" {
		return s.subscriberID, nil
	}

	id, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	s.subscriberID = id
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	return s.subscriberID, nil
}

func (s *Store) AddDecision(d Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.AtUTC == "" {
		d.AtUTC = time.Now().UTC().Format(time.RFC3339)
	}
	s.decisions = append(s.decisions, d)
	return s.persistDecisionsLocked()
}

func (s *Store) Decisions(limit int) []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decisions) == 0 {
		return []Decision{}
	}
	out := make([]Decision, len(s.decisions))
	copy(out, s.decisions)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AtUTC > out[j].AtUTC
	})
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func (s *Store) ProcessedSince(since time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, processedAt := range s.processedSet {
		if !processedAt.Before(since) {
			count++
		}
	}
	return count
}

func (s *Store) persistLocked() error {
	if !s.notificationsDirty {
		if err := s.refreshNotificationsFromDiskLocked(); err != nil {
			return err
		}
	}

	processed := make(map[string]string, len(s.processedSet))
	for id, ts := range s.processedSet {
		processed[id] = ts.Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(stateFile{
		LastCheckpoint:       s.checkpoint,
		Processed:            processed,
		Notifications:        s.notifications,
		SubscriberID:         s.subscriberID,
		AICreditsExhausted:   s.aiCreditsExhausted,
		AICreditsExhaustedAt: s.aiCreditsExhaustedAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path(), b, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	s.notificationsDirty = false
	return nil
}

func (s *Store) ListNotificationSubscriptions() []NotificationSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.refreshNotificationsFromDiskLocked()
	out := make([]NotificationSubscription, len(s.notifications))
	copy(out, s.notifications)
	return out
}

func (s *Store) UpsertNotificationSubscription(sub NotificationSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshStateFromDiskLocked(); err != nil {
		return err
	}
	for i, existing := range s.notifications {
		if existing.Endpoint == sub.Endpoint {
			s.notifications[i] = sub
			s.notificationsDirty = true
			return s.persistLocked()
		}
	}
	s.notifications = append(s.notifications, sub)
	s.notificationsDirty = true
	return s.persistLocked()
}

func (s *Store) RemoveNotificationSubscription(endpoint string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshStateFromDiskLocked(); err != nil {
		return false, err
	}
	for i, sub := range s.notifications {
		if sub.Endpoint != endpoint {
			continue
		}
		s.notifications = append(s.notifications[:i], s.notifications[i+1:]...)
		s.notificationsDirty = true
		if err := s.persistLocked(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// SetAICreditsExhausted marks that Llama reported the weekly chat limit / out of
// AI credits. It returns true only on the false->true transition so callers can
// notify exactly once until the flag is reset. The flag is persisted so it
// survives a restart.
func (s *Store) SetAICreditsExhausted(atUTC string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aiCreditsExhausted {
		return false, nil
	}
	if strings.TrimSpace(atUTC) == "" {
		atUTC = time.Now().UTC().Format(time.RFC3339)
	}
	s.aiCreditsExhausted = true
	s.aiCreditsExhaustedAt = atUTC
	return true, s.persistLocked()
}

// ClearAICreditsExhausted resets the AI-credits flag. It returns true only when
// the flag was previously set (true->false transition).
func (s *Store) ClearAICreditsExhausted() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aiCreditsExhausted {
		return false, nil
	}
	s.aiCreditsExhausted = false
	s.aiCreditsExhaustedAt = ""
	return true, s.persistLocked()
}

// AICreditsExhausted reports whether the AI-credits flag is set and when it was
// first raised.
func (s *Store) AICreditsExhausted() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aiCreditsExhausted, s.aiCreditsExhaustedAt
}

func (s *Store) loadDecisions() error {
	b, err := os.ReadFile(s.decisionsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.persistDecisionsLocked()
		}
		return err
	}
	var d []Decision
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	s.decisions = d
	return nil
}

func (s *Store) persistDecisionsLocked() error {
	b, err := json.MarshalIndent(s.decisions, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.decisionsPath(), b, 0o600); err != nil {
		return fmt.Errorf("write decisions: %w", err)
	}
	return nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
