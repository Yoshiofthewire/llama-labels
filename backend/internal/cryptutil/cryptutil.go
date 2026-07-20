// Package cryptutil provides the shared AES-GCM "envelope" encrypt/decrypt
// primitives used to protect credentials and other sensitive payloads at
// rest (IMAP credentials, notification keys, etc). It is a leaf package with
// no dependencies on the rest of this codebase besides fsutil.
package cryptutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"kypost-server/backend/internal/fsutil"
)

// EncryptedPayload is the on-disk envelope format for an AES-GCM sealed
// payload.
type EncryptedPayload struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// ParseEnvelope parses raw as an EncryptedPayload. ok is false when raw does
// not unmarshal as a well-formed version-1 envelope (missing nonce/
// ciphertext, or an unrecognized version) — callers use this to fall back to
// treating raw as plaintext for backward compatibility with data written
// before encryption-at-rest was introduced.
func ParseEnvelope(raw []byte) (EncryptedPayload, bool) {
	var env EncryptedPayload
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != 1 || strings.TrimSpace(env.Nonce) == "" || strings.TrimSpace(env.Ciphertext) == "" {
		return EncryptedPayload{}, false
	}
	return env, true
}

// LoadOrCreateKey reads the 32-byte base64-encoded master key at path,
// generating and persisting a new random one via fsutil.AtomicWriteFile if
// the file does not yet exist. Only callers that are allowed to originate
// the master key (the api process) should use this; other processes must
// use LoadKey so they never race-create a key the owning process didn't
// expect.
func LoadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(b)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	if err := fsutil.AtomicWriteFile(path, encoded, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// LoadKey reads the 32-byte base64-encoded master key at path. Unlike
// LoadOrCreateKey, it never creates the file — it errors if the key is
// missing.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeKey(b)
}

func decodeKey(raw []byte) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	if len(decoded) != 32 {
		return nil, errors.New("invalid encryption master key length")
	}
	return decoded, nil
}

// Seal AES-GCM encrypts payload with key, returning the envelope ready to be
// marshaled to disk.
func Seal(payload, key []byte) (EncryptedPayload, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return EncryptedPayload{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedPayload{}, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return EncryptedPayload{}, err
	}

	sealed := gcm.Seal(nil, nonce, payload, nil)
	return EncryptedPayload{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// SealString AES-GCM seals plaintext with the master key at keyPath
// (creating the key on first use) and returns the JSON envelope as a
// string, ready to persist. Shared by every caller that seals a single
// secret string as an encrypted-at-rest envelope (PGP private keys, TOTP
// secrets).
func SealString(plaintext, keyPath string) (string, error) {
	key, err := LoadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	env, err := Seal([]byte(plaintext), key)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// OpenString reverses SealString, returning the plaintext. errNotEnvelope is
// returned verbatim when enc isn't a well-formed envelope, so callers can
// supply their own contextual message.
func OpenString(enc, keyPath string, errNotEnvelope error) (string, error) {
	env, ok := ParseEnvelope([]byte(enc))
	if !ok {
		return "", errNotEnvelope
	}
	key, err := LoadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	plain, err := Open(env, key)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// Open AES-GCM decrypts env with key.
func Open(env EncryptedPayload, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}
