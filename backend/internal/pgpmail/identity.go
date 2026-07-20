// Package pgpmail implements OpenPGP encryption, signing, and key-identity
// management for the mail send/receive paths. All private key material is
// held in memory only for the duration of a request; callers persist it via
// SealPrivateKey (an AES-GCM envelope, the same pattern as
// mfa.SealTOTPSecret) and never write unsealed key material to disk.
package pgpmail

import (
	"errors"
	"fmt"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"kypost-server/backend/internal/cryptutil"
)

// Identity holds one OpenPGP keypair loaded in memory: a user's own private
// identity, used for decrypting and signing. Recipients' public keys are
// passed around as plain armored strings (Contact.PGPKey) since they never
// need decrypt/sign and so never need this type.
type Identity struct {
	Fingerprint      string
	KeyID            string
	ArmoredPublicKey string

	key *crypto.Key
}

// GenerateIdentity creates a new OpenPGP keypair for name/email using
// gopenpgp's default profile (EdDSA/Curve25519 + SHA256, RFC4880-compatible
// and interoperable with the openpgp.js keys already used client-side for
// contacts).
func GenerateIdentity(name, email string) (*Identity, error) {
	keyGen := crypto.PGP().KeyGeneration().AddUserId(name, email).New()
	key, err := keyGen.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: generate key: %w", err)
	}
	return identityFromKey(key)
}

// ImportIdentity parses an armored private key, unlocking it with passphrase
// if it is passphrase-protected (pass "" for an unprotected key).
func ImportIdentity(armoredPrivateKey, passphrase string) (*Identity, error) {
	key, err := crypto.NewPrivateKeyFromArmored(armoredPrivateKey, []byte(passphrase))
	if err != nil {
		return nil, fmt.Errorf("pgpmail: unlock private key: %w", err)
	}
	if !key.IsPrivate() {
		return nil, errors.New("pgpmail: armored key does not contain private key material")
	}
	return identityFromKey(key)
}

func identityFromKey(key *crypto.Key) (*Identity, error) {
	armoredPub, err := key.GetArmoredPublicKey()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: armor public key: %w", err)
	}
	return &Identity{
		Fingerprint:      key.GetFingerprint(),
		KeyID:            key.GetHexKeyID(),
		ArmoredPublicKey: armoredPub,
		key:              key,
	}, nil
}

// SealPrivateKey AES-GCM seals the identity's private key with the master
// key at keyPath (creating the key on first use) and returns the JSON
// envelope as a string, ready to store on User.PGPPrivateKeyEnc. The armored
// form stored inside the envelope is unprotected (no passphrase) — the
// envelope's AES-GCM key is the sole protection, matching how
// mfa.SealTOTPSecret protects TOTP secrets.
func (id *Identity) SealPrivateKey(keyPath string) (string, error) {
	armored, err := id.key.Armor()
	if err != nil {
		return "", fmt.Errorf("pgpmail: armor private key: %w", err)
	}
	return cryptutil.SealString(armored, keyPath)
}

// OpenPrivateKey reverses SealPrivateKey, returning a usable Identity.
// Mirrors mfa.OpenTOTPSecret.
func OpenPrivateKey(enc, keyPath string) (*Identity, error) {
	armored, err := cryptutil.OpenString(enc, keyPath, errors.New("pgpmail: private key is not a valid envelope"))
	if err != nil {
		return nil, err
	}
	unlockedKey, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: parse stored private key: %w", err)
	}
	return identityFromKey(unlockedKey)
}
