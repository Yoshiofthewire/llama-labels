package pgpmail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"llama-lab/backend/internal/fsutil"
)

// SignatureRecord is the verification outcome for one received signed
// message, keyed by MessageID (this codebase's existing convention —
// strconv.Itoa(UID), see mailcache.Entry — not the RFC822 Message-ID
// header) so the frontend can render a badge without re-verifying on every
// view.
type SignatureRecord struct {
	MessageID         string `json:"messageId"`
	SignerFingerprint string `json:"signerFingerprint"`
	Verified          bool   `json:"verified"`
	VerifiedAt        string `json:"verifiedAt"`
	Note              string `json:"note,omitempty"`
}

type sigStoreFile struct {
	Version int                        `json:"version"`
	Records map[string]SignatureRecord `json:"records"`
}

// SigStore is the per-user store of inbound signature verification records,
// persisted as a sibling file next to the user's other per-user JSON state
// (contacts.json, state.json).
type SigStore struct {
	mu   sync.Mutex
	path string
}

// NewSigStore opens (without requiring it to exist yet) the signature store
// at baseDir/pgp-signatures.json.
func NewSigStore(baseDir string) *SigStore {
	return &SigStore{path: filepath.Join(baseDir, "pgp-signatures.json")}
}

func (s *SigStore) load() (sigStoreFile, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return sigStoreFile{Version: 1, Records: map[string]SignatureRecord{}}, nil
	}
	if err != nil {
		return sigStoreFile{}, err
	}
	var f sigStoreFile
	if err := json.Unmarshal(b, &f); err != nil {
		return sigStoreFile{}, err
	}
	if f.Records == nil {
		f.Records = map[string]SignatureRecord{}
	}
	return f, nil
}

func (s *SigStore) save(f sigStoreFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(s.path, b, 0o600)
}

// Put records the verification outcome for a message, overwriting any
// existing record for the same MessageID.
func (s *SigStore) Put(record SignatureRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Records[record.MessageID] = record
	return s.save(f)
}

// Get returns the verification record for a MessageID, if one exists.
func (s *SigStore) Get(messageID string) (SignatureRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return SignatureRecord{}, false
	}
	rec, ok := f.Records[messageID]
	return rec, ok
}
