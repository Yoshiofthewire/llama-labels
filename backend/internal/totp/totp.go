// Package totp implements RFC 6238 time-based one-time passwords using only
// the Go standard library (HMAC-SHA1, 6 digits, 30-second steps). It has no
// dependencies on the rest of this codebase.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	secretBytes = 20 // RFC 4226/6238 recommended shared-secret length
	period      = 30 // seconds per step
	digits      = 6
)

// b32 is the RFC 4648 base32 alphabet with no padding — the encoding
// authenticator apps expect for otpauth secrets.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a fresh 20-byte random secret, base32-encoded
// (no padding).
func GenerateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// ProvisioningURI builds the otpauth:// URI encoded into the enrollment QR
// code. issuer and accountName are shown by the authenticator app.
func ProvisioningURI(issuer, accountName, base32Secret string) string {
	label := url.PathEscape(issuer + ":" + accountName)
	q := url.Values{}
	q.Set("secret", base32Secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(digits))
	q.Set("period", strconv.Itoa(period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Validate reports whether code is a valid TOTP for base32Secret at time t,
// accepting a one-step skew on either side (t-30s, t, t+30s). On success it
// returns the matched step counter (Unix seconds / 30), which callers use to
// pin down which one-time code was consumed for replay tracking.
func Validate(base32Secret, code string, t time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != digits {
		return 0, false
	}
	step := t.Unix() / period
	for _, delta := range []int64{-1, 0, 1} {
		counter := step + delta
		if counter < 0 {
			continue
		}
		expected, err := generateCode(base32Secret, uint64(counter))
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

// generateCode computes the HMAC-SHA1 dynamic-truncation TOTP for a specific
// counter, per RFC 4226 section 5.3.
func generateCode(base32Secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(base32Secret)))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)

	const mod = 1_000_000 // 10^digits
	return fmt.Sprintf("%0*d", digits, bin%mod), nil
}
