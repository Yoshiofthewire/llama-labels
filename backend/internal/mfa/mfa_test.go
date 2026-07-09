package mfa

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
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

func TestConsumeTOTPStep(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.ConsumeTOTPStep(ch.ID, 987654); err != nil {
		t.Fatalf("first ConsumeTOTPStep = %v, want nil", err)
	}
	got, ok := s.Get(ch.ID)
	if !ok || got.UsedTOTPStep != 987654 {
		t.Fatalf("UsedTOTPStep = %d (ok=%v), want 987654", got.UsedTOTPStep, ok)
	}

	// A second consume against the same challenge — whether with the same
	// step (replay) or a different one — must be rejected.
	if err := s.ConsumeTOTPStep(ch.ID, 987654); !errors.Is(err, ErrChallengeAlreadyUsed) {
		t.Fatalf("second ConsumeTOTPStep (same step) = %v, want ErrChallengeAlreadyUsed", err)
	}
	if err := s.ConsumeTOTPStep(ch.ID, 111111); !errors.Is(err, ErrChallengeAlreadyUsed) {
		t.Fatalf("second ConsumeTOTPStep (different step) = %v, want ErrChallengeAlreadyUsed", err)
	}

	if err := s.ConsumeTOTPStep("no-such-challenge", 1); !errors.Is(err, ErrChallengeNotFound) {
		t.Fatalf("ConsumeTOTPStep on unknown id = %v, want ErrChallengeNotFound", err)
	}
}

// TestConsumeTOTPStepConcurrentSingleUse is the regression test for the
// TOCTOU race this method exists to close: many goroutines racing to consume
// the same still-valid TOTP code against the same challenge must yield
// exactly one winner, with every other caller told the challenge was already
// used. Run with -race to also catch any reintroduced data race.
func TestConsumeTOTPStepConcurrentSingleUse(t *testing.T) {
	s := NewStore()
	ch, err := s.Create("user-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, alreadyUsed, other int

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := s.ConsumeTOTPStep(ch.ID, 555555)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrChallengeAlreadyUsed):
				alreadyUsed++
			default:
				other++
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (successes=%d alreadyUsed=%d other=%d)", successes, successes, alreadyUsed, other)
	}
	if alreadyUsed != goroutines-1 {
		t.Fatalf("alreadyUsed = %d, want %d (successes=%d alreadyUsed=%d other=%d)", alreadyUsed, goroutines-1, successes, alreadyUsed, other)
	}
	if other != 0 {
		t.Fatalf("unexpected other errors = %d", other)
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
