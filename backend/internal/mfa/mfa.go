// Package mfa holds the multi-factor-auth business logic that is independent
// of net/http: the in-memory login-challenge store, recovery-code generation,
// and TOTP-secret sealing on top of cryptutil.
package mfa

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"llama-lab/backend/internal/cryptutil"
)

// MaxTOTPAttempts is the number of failed second-factor attempts tolerated on
// a single login challenge before it is invalidated.
const MaxTOTPAttempts = 5

// challengeTTL is how long a login challenge stays valid after password
// verification while the user produces their second factor.
const challengeTTL = 5 * time.Minute

var (
	ErrChallengeNotFound = errors.New("mfa: challenge not found")
	ErrTooManyAttempts   = errors.New("mfa: too many attempts")

	// ErrChallengeAlreadyUsed indicates a TOTP code was already consumed
	// against this challenge — returned by ConsumeTOTPStep on a replay
	// attempt.
	ErrChallengeAlreadyUsed = errors.New("mfa: challenge already used")
)

// Challenge is an in-progress second-factor login. It exists between a
// successful password check and a successful (or exhausted) TOTP/recovery
// verification.
type Challenge struct {
	ID           string
	UserID       string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	TOTPAttempts int
	UsedTOTPStep int64
}

// Store is a concurrency-safe in-memory challenge map. Entries are swept
// lazily on access (mirroring the api server's session expiry), which is
// sufficient given the short TTL and the fact that every challenge is created
// only to be consumed within seconds.
type Store struct {
	mu sync.Mutex
	m  map[string]Challenge
}

func NewStore() *Store {
	return &Store{m: map[string]Challenge{}}
}

// Create mints a new challenge for userID with a fresh random ID.
func (s *Store) Create(userID string) (Challenge, error) {
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		return Challenge{}, err
	}
	now := time.Now()
	ch := Challenge{
		ID:        hex.EncodeToString(idBytes),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(challengeTTL),
	}
	s.mu.Lock()
	s.m[ch.ID] = ch
	s.mu.Unlock()
	return ch, nil
}

// Get returns the live challenge for id, lazily deleting and reporting
// ok=false if it is missing or expired.
func (s *Store) Get(id string) (Challenge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.m[id]
	if !ok {
		return Challenge{}, false
	}
	if time.Now().After(ch.ExpiresAt) {
		delete(s.m, id)
		return Challenge{}, false
	}
	return ch, true
}

// RecordTOTPAttempt increments the failed-attempt counter. It returns
// ErrChallengeNotFound when the challenge is unknown or expired, and
// ErrTooManyAttempts (after deleting the challenge) once the count exceeds
// MaxTOTPAttempts.
func (s *Store) RecordTOTPAttempt(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.m[id]
	if !ok || time.Now().After(ch.ExpiresAt) {
		delete(s.m, id)
		return ErrChallengeNotFound
	}
	ch.TOTPAttempts++
	if ch.TOTPAttempts > MaxTOTPAttempts {
		delete(s.m, id)
		return ErrTooManyAttempts
	}
	s.m[id] = ch
	return nil
}

// ConsumeTOTPStep atomically checks whether this challenge has already had a
// TOTP step consumed and, if not, marks it consumed with step in the same
// locked critical section. Callers must call this only after totp.Validate
// has confirmed step is a currently-valid code — ConsumeTOTPStep itself does
// not validate the code, it only enforces single-use. Doing the check and the
// write under one lock (rather than a separate Get + later RecordTOTPStep)
// closes a TOCTOU window where two concurrent requests bearing the same valid
// code could otherwise both pass a stale "not yet used" check before either
// recorded its use.
func (s *Store) ConsumeTOTPStep(id string, step int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.m[id]
	if !ok || time.Now().After(ch.ExpiresAt) {
		delete(s.m, id)
		return ErrChallengeNotFound
	}
	if ch.UsedTOTPStep != 0 {
		return ErrChallengeAlreadyUsed
	}
	ch.UsedTOTPStep = step
	s.m[id] = ch
	return nil
}

// Delete removes a challenge (called on success or lockout).
func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// recoveryAlphabet has exactly 32 characters so a random byte modulo 32 is
// unbiased. It omits visually ambiguous characters is not required here since
// codes are copy/pasted, not transcribed by hand.
const recoveryAlphabet = "0123456789abcdefghijklmnopqrstuv"

// GenerateRecoveryCodes returns n one-time recovery codes formatted
// xxxx-xxxx-xxxx using crypto/rand.
func GenerateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 12) // 3 groups of 4 chars
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		chars := make([]byte, 12)
		for j, b := range raw {
			chars[j] = recoveryAlphabet[int(b)%len(recoveryAlphabet)]
		}
		codes = append(codes, string(chars[0:4])+"-"+string(chars[4:8])+"-"+string(chars[8:12]))
	}
	return codes, nil
}

// SealTOTPSecret AES-GCM seals base32Secret with the key at keyPath (creating
// the key on first use) and returns the JSON envelope as a string, ready to
// store on User.TOTPSecretEnc.
func SealTOTPSecret(base32Secret, keyPath string) (string, error) {
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	env, err := cryptutil.Seal([]byte(base32Secret), key)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// OpenTOTPSecret reverses SealTOTPSecret, returning the base32 secret.
func OpenTOTPSecret(enc, keyPath string) (string, error) {
	env, ok := cryptutil.ParseEnvelope([]byte(enc))
	if !ok {
		return "", errors.New("mfa: totp secret is not a valid envelope")
	}
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	plain, err := cryptutil.Open(env, key)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
