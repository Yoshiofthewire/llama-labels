package mfa

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChallengeCreateGetDelete(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ch.ID == "" || ch.UserID != "user-1" {
		t.Fatalf("unexpected challenge: %+v", ch)
	}
	got, ok := s.Get(ch.ID)
	if !ok || got.ID != ch.ID {
		t.Fatalf("Get after Create = (%+v, %v)", got, ok)
	}
	s.Delete(ch.ID)
	if _, ok := s.Get(ch.ID); ok {
		t.Fatalf("expected challenge gone after Delete")
	}
}

func TestChallengeExpiry(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Force the stored entry to be already expired.
	s.mu.Lock()
	entry := s.m[ch.ID]
	entry.ExpiresAt = time.Now().Add(-time.Second)
	s.m[ch.ID] = entry
	s.mu.Unlock()

	if _, ok := s.Get(ch.ID); ok {
		t.Fatalf("expected expired challenge to be rejected by Get")
	}
	if err := s.RecordTOTPAttempt(ch.ID); !errors.Is(err, ErrChallengeNotFound) {
		t.Fatalf("RecordTOTPAttempt on expired = %v, want ErrChallengeNotFound", err)
	}
}

func TestRecordTOTPAttemptLockout(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// MaxTOTPAttempts failures are tolerated; the next one locks out.
	for i := 0; i < MaxTOTPAttempts; i++ {
		if err := s.RecordTOTPAttempt(ch.ID); err != nil {
			t.Fatalf("attempt %d = %v, want nil", i+1, err)
		}
	}
	if err := s.RecordTOTPAttempt(ch.ID); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("attempt over cap = %v, want ErrTooManyAttempts", err)
	}
	// Lockout deletes the challenge.
	if _, ok := s.Get(ch.ID); ok {
		t.Fatalf("expected challenge deleted after lockout")
	}
}

func TestRecordTOTPStep(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.RecordTOTPStep(ch.ID, 987654)
	got, ok := s.Get(ch.ID)
	if !ok || got.UsedTOTPStep != 987654 {
		t.Fatalf("UsedTOTPStep = %d (ok=%v), want 987654", got.UsedTOTPStep, ok)
	}
}

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("len = %d, want 10", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("duplicate recovery code %q", c)
		}
		seen[c] = true
		parts := strings.Split(c, "-")
		if len(parts) != 3 {
			t.Fatalf("code %q not xxxx-xxxx-xxxx", c)
		}
		for _, p := range parts {
			if len(p) != 4 {
				t.Fatalf("code %q group wrong length", c)
			}
		}
	}
}

func TestSealOpenTOTPSecretRoundTrip(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "totp-secret.key")
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

	enc, err := SealTOTPSecret(secret, keyPath)
	if err != nil {
		t.Fatalf("SealTOTPSecret: %v", err)
	}
	if enc == secret || !strings.Contains(enc, "ciphertext") {
		t.Fatalf("sealed value looks wrong: %q", enc)
	}
	got, err := OpenTOTPSecret(enc, keyPath)
	if err != nil {
		t.Fatalf("OpenTOTPSecret: %v", err)
	}
	if got != secret {
		t.Fatalf("round trip = %q, want %q", got, secret)
	}
}
