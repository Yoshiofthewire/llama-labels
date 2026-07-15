package pgpmail

import (
	"fmt"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// KeyStatus reports whether an OpenPGP key is currently usable for
// encryption or signing, as of the moment it was computed. It is never
// cached — callers recompute it from the armored key text each time they
// need it, since a key's revocation/expiry status can change between calls.
type KeyStatus struct {
	Revoked bool
	Expired bool
}

// Usable reports whether a key in this status can be used for encryption
// or signing right now. It does not affect decryption — a user must still
// be able to read old mail after their own key is revoked or expires.
func (s KeyStatus) Usable() bool {
	return !s.Revoked && !s.Expired
}

// CheckKeyStatus parses an armored OpenPGP key (public or private) and
// reports its revocation/expiry status as of now.
func CheckKeyStatus(armoredKey string) (KeyStatus, error) {
	key, err := crypto.NewKeyFromArmored(armoredKey)
	if err != nil {
		return KeyStatus{}, fmt.Errorf("pgpmail: parse key: %w", err)
	}
	return keyStatusOf(key), nil
}

func keyStatusOf(key *crypto.Key) KeyStatus {
	now := time.Now().Unix()
	return KeyStatus{
		Revoked: key.IsRevoked(now),
		Expired: key.IsExpired(now),
	}
}

// Status reports id's current revocation/expiry status as of now.
func (id *Identity) Status() KeyStatus {
	return keyStatusOf(id.key)
}
