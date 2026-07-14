# PGP BCC leak fix + key revocation/expiry enforcement

## Problem

Two gaps found while reviewing the PGP/MIME send path (`backend/internal/api/server.go`, `backend/internal/pgpmail/`):

1. **BCC confidentiality leak under encryption.** `handleMailSend`'s encrypted branch merges To + CC + BCC recipients' keys into a single `crypto.PGP().Encryption().Recipients(...)` keyring and sends one shared ciphertext to all of them in one SMTP transaction. OpenPGP embeds each recipient's key ID in a PKESK packet per recipient key, so any To/CC recipient can inspect the message's packet structure (without decrypting) and see that a BCC'd recipient's key was also a target — deanonymizing the BCC. The `Bcc:` header itself is correctly stripped (`mailmsg.go:63-64`); the leak is in the shared ciphertext, not the header.
2. **No revocation or expiry enforcement.** Nothing in `pgpmail` or the API checks `Key.IsRevoked()`/`Key.IsExpired()` before encrypting to a contact's key or signing with the user's own identity. A recipient's lapsed key is silently treated as valid; a user can keep signing mail with a revoked identity indefinitely.

## Goals

- BCC recipients never share a ciphertext (or SMTP transaction) with To/CC recipients or with each other.
- Revoked/expired recipient keys are treated the same as "no key on file" (falls back to the existing plaintext pickup-link notification) rather than silently used or hard-blocking the send.
- A revoked/expired own identity blocks `Sign=true` sends with a clear error; `Encrypt=true` alone is unaffected since it only depends on recipients' keys.
- Key status becomes visible (not just enforced) via three read endpoints, so clients can warn before the user hits a hard error.
- No schema/storage changes — status is computed live from already-stored armored key text.

## Non-goals

- No caching of a revoked/expired flag on `Contact` or `User`. Status is always recomputed from the armored key at time of use via gopenpgp's `Key.IsRevoked(now)`/`Key.IsExpired(now)`.
- No change to decryption behavior. A user must still be able to decrypt old mail after their own key expires or is revoked — decryption is not gated on key validity, matching gopenpgp's own behavior (`Decrypt()` isn't capability-flag-gated the way `CanEncrypt`/`CanSign`/`CanVerify` are).
- No change to signature verification on the receive path (`pgp_receive.go`, `VerifyDetached`) — out of scope for this change; only send-time signing and recipient-key selection are affected.
- No frontend changes. This spec covers backend enforcement and the additive JSON fields; consuming them in the UI is separate work.

## Design

### 1. `pgpmail.KeyStatus` — shared status helper

New type and functions in the `pgpmail` package (new file `keystatus.go`):

```go
// KeyStatus reports whether an OpenPGP key is currently usable.
type KeyStatus struct {
    Revoked bool
    Expired bool
}

// Usable reports whether the key can be used for encryption/signing right now.
func (s KeyStatus) Usable() bool { return !s.Revoked && !s.Expired }

// CheckKeyStatus parses an armored public key and reports its current
// revocation/expiry status as of now.
func CheckKeyStatus(armoredKey string) (KeyStatus, error)

// Status reports id's current revocation/expiry status as of now.
func (id *Identity) Status() KeyStatus
```

Both use `time.Now().Unix()` against `Key.IsRevoked(unixTime)` / `Key.IsExpired(unixTime)` (gopenpgp v3, confirmed present in `crypto/key.go`). `CheckKeyStatus` parses the armored string fresh each call (same cost as the existing `crypto.NewKeyFromArmored` calls already scattered through this package); `Identity.Status()` reuses the already-parsed `*crypto.Key` held internally.

This is the single source of truth every enforcement/surfacing point below calls into.

### 2. BCC fix — per-recipient ciphertext and delivery

In `handleMailSend`'s encrypted branch (`server.go:792-833`):

- The recipient-key-lookup loop (currently building one `withKeyEmails`/`recipientKeys` pair across To+CC+BCC) is split so BCC is tracked separately from To/CC. Each usable BCC key is paired with its address.
- **To/CC path (unchanged in spirit):** if any To/CC recipient has a usable key, one shared `EncryptMIME` call encrypts to just those keys, sent as one SMTP transaction to just those addresses — same as today, minus BCC's contribution to the shared keyring.
- **BCC path (new):** for each BCC recipient with a usable key, a separate `EncryptMIME` call encrypts to only that recipient's key (with the same signer per `encryptSigner`), delivered via its own SMTP transaction to just that one address. No BCC recipient's key ID appears in any ciphertext another recipient (To, CC, or another BCC) can inspect.
- **Delivery helper:** the raw SMTP-send logic currently inlined in `finishMailSend` (`server.go:875-886`) is extracted into a small reusable function (e.g. `smtpDeliver(smtpHost, smtpPort, addr, smtpUsername, smtpPassword, from string, recipients []string, msg []byte) error`) so both the main send and the per-BCC sends share it without duplicating the port-465-vs-STARTTLS branching.
- **Edge case:** if no To/CC recipient has a usable key but at least one BCC recipient does, `finishMailSend` must not attempt an SMTP transaction with zero recipients — guard the call to `smtpDeliver` on `len(recipients) > 0`. The Sent-folder save and JSON response still happen exactly once regardless of how the send was split, since `finishMailSend`'s `SaveSent` call already only uses `toList`/`ccList`/`bccList`/`req.Subject`/`req.Body` (the original plaintext request fields) — never the ciphertext or the SMTP recipient list — so it needs no changes to stay correct under this split.
- **Failure handling:** a failed per-BCC send is logged (`s.logger.Error`) and skipped, not surfaced as a request failure — matches the existing best-effort pattern used for pickup notifications (`server.go:829-833`). The main To/CC send failing is still a hard error, as today.
- Recipients (To, CC, or BCC) whose key turns out unusable (no key, or revoked/expired — see below) all land in the existing `withoutKeyEmails` bucket and get the existing plaintext pickup-link notification, unchanged.

### 3. Revocation/expiry enforcement

- **Recipient key filtering** (`server.go:797-812`): after `findContactPGPKey` returns a key, call `pgpmail.CheckKeyStatus(key)`. Only add to the usable (with-key) set if it returns no error and `Usable()` is true. A parse error or unusable status routes the recipient to `withoutKeyEmails`, same as no key at all.
- **Own identity, signing only** (`server.go:763-776`): once `signer` is loaded, if `req.Sign` is true and `signer.Status().Usable()` is false, respond 400 with a clear message (e.g. "cannot sign — your pgp identity is revoked or expired, generate or import a new one") and stop. `req.Encrypt` alone (no `Sign`) is unaffected — `encryptSigner` already returns `nil` when `!req.Sign`, so a revoked/expired own identity never silently taints an encrypt-only send.
- **Decryption:** no changes. Confirmed gopenpgp does not gate `Decrypt()` on `IsRevoked`/`IsExpired`, so the receive path (`pgp_receive.go`) needs no special-casing to keep working.

### 4. Surfacing status (additive JSON fields, no behavior change)

- `GET /api/pgp/identity` (`pgp_handlers.go:90-113`): `pgpIdentityResponse` gains `revoked`/`expired` bool fields, computed via `pgpmail.CheckKeyStatus(u.PGPPublicKey)` when building the response.
- `GET /api/pgp/keyserver/lookup` (`pgp_keyserver.go:24-73`): the response gains `revoked`/`expired` bool fields, computed from the already-parsed `key` variable (`key.IsRevoked(now)`/`key.IsExpired(now)`) — no extra parse needed.
- `POST /api/pgp/recipients/check` (`pgp_keyserver.go:78-108`): `addressStatus` gains `revoked`/`expired` bool fields alongside `hasKey`, computed the same way as the send-path filtering in §3 so the two stay consistent (a recipient reported `hasKey: true` here must be the same recipient the send path would treat as usable).

## Testing

- `pgpmail` package: table tests for `CheckKeyStatus` and `Identity.Status` covering valid, expired, and revoked keys (generate short-lived and revoked keys in-test, e.g. via gopenpgp's key-generation with a short expiry and a self-revocation signature — check existing test fixtures in `mime_test.go`/`identity_test.go` first for reusable helpers before writing new ones).
- `api` package:
  - Encrypted send with To=2 (one with key, one without), CC=1 (with key), BCC=2 (both with keys) produces: one shared ciphertext/SMTP transaction to the To+CC key-holder, two separate ciphertexts/SMTP transactions to each BCC key-holder, and one pickup notification to the keyless To recipient.
  - A contact with a revoked (or expired) key is routed to the pickup path instead of being encrypted to.
  - `Sign=true` with a revoked/expired own identity returns 400; `Encrypt=true` alone with the same identity still succeeds.
  - `GET /api/pgp/identity`, `GET /api/pgp/keyserver/lookup`, and `POST /api/pgp/recipients/check` correctly report `revoked`/`expired` for known-bad keys.
