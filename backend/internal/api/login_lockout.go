package api

import (
	"sync"
	"time"

	"kypost-server/backend/internal/users"
)

// loginMaxFailures/loginLockoutFor implement a three-strikes, 15-minute
// cooldown on password login: after loginMaxFailures failed attempts for a
// given username+client IP pair, further attempts for that pair are rejected
// until loginLockoutFor has elapsed. This is independent of whether the
// username actually exists — see failureLockout.allowed — so it can't be
// used to distinguish valid from invalid usernames by lockout behavior; and
// it is scoped to the client IP so failures manufactured by an attacker
// can't lock the account's real owner out from their own machine.
const (
	loginMaxFailures = 3
	loginLockoutFor  = 15 * time.Minute

	// davMaxFailures/davLockoutFor guard the CardDAV Basic Auth surface,
	// keyed by client IP (usernames there are fixed account names, and the
	// password is server-generated — the realistic abuse is one host burning
	// CPU with scrypt verifications, not a credible guessing campaign). The
	// threshold is looser than login's because sync clients legitimately
	// retry a stale password several times before surfacing an error.
	davMaxFailures = 10
	davLockoutFor  = 15 * time.Minute

	// mfaMaxFailures/mfaLockoutFor throttle second-factor verification per
	// account across challenges. The per-challenge attempt cap (mfa.Store) is
	// not enough on its own: a password-holding attacker can mint an unlimited
	// number of fresh challenges (each valid-password login clears the login
	// lockout), so without an account-scoped counter the TOTP code is brute
	// forceable online. Keyed on the challenge's UserID.
	mfaMaxFailures = 10
	mfaLockoutFor  = 15 * time.Minute

	// loginLockoutSweepThreshold bounds how large loginLockout.entries can
	// grow before a housekeeping sweep runs. An attacker submitting a stream
	// of distinct, nonexistent usernames each gets its own entry that never
	// reaches the lockout threshold and is otherwise never removed —
	// unbounded memory growth over a sustained attack. Sweeping out every
	// not-currently-locked entry once the map gets this large keeps memory
	// bounded without a background goroutine; legitimate locked-out entries
	// (the ones actually worth remembering) are untouched.
	loginLockoutSweepThreshold = 10_000
)

type loginLockoutEntry struct {
	failures    int
	lockedUntil time.Time
}

// failureLockout is small in-memory, keyed strike/cooldown state: after
// maxFailures failed attempts for a key, further attempts for that key are
// rejected until lockoutFor has elapsed. handleLogin keys it by
// username+client IP; withDAVBasicAuth keys it by client IP alone. It
// intentionally lives outside Server.sessions/mu — it's unrelated state with
// its own, much smaller lock scope.
type failureLockout struct {
	mu          sync.Mutex
	maxFailures int
	lockoutFor  time.Duration
	entries     map[string]*loginLockoutEntry
}

func newFailureLockout(maxFailures int, lockoutFor time.Duration) *failureLockout {
	return &failureLockout{
		maxFailures: maxFailures,
		lockoutFor:  lockoutFor,
		entries:     map[string]*loginLockoutEntry{},
	}
}

func newLoginLockout() *failureLockout {
	return newFailureLockout(loginMaxFailures, loginLockoutFor)
}

// allowed reports whether username may attempt a login right now. When
// false, retryAfter is how much longer the lockout has to run.
func (l *failureLockout) allowed(username string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, exists := l.entries[username]
	if !exists {
		return true, 0
	}
	if remaining := time.Until(e.lockedUntil); remaining > 0 {
		return false, remaining
	}
	return true, 0
}

// recordFailure counts one failed attempt for username, locking it out for
// loginLockoutFor once it reaches loginMaxFailures. A lockout that has
// already expired resets the strike count first, so failures don't
// accumulate forever across unrelated attempts long after the last lockout.
func (l *failureLockout) recordFailure(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= loginLockoutSweepThreshold {
		now := time.Now()
		for k, e := range l.entries {
			if e.lockedUntil.IsZero() || !now.Before(e.lockedUntil) {
				delete(l.entries, k)
			}
		}
	}
	e, exists := l.entries[username]
	if !exists {
		e = &loginLockoutEntry{}
		l.entries[username] = e
	} else if !e.lockedUntil.IsZero() && !time.Now().Before(e.lockedUntil) {
		e.failures = 0
		e.lockedUntil = time.Time{}
	}
	e.failures++
	if e.failures >= l.maxFailures {
		e.lockedUntil = time.Now().Add(l.lockoutFor)
	}
}

// recordSuccess clears any strike history for username, so a successful
// login always starts the next set of attempts with a clean slate.
func (l *failureLockout) recordSuccess(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, username)
}

var (
	timingDummyHashOnce sync.Once
	timingDummyHash     string
)

// equalizeLoginTiming verifies candidate against a throwaway scrypt hash so
// the unknown-username (and inactive-account) login path costs the same as a
// real wrong-password check. The dummy hash is minted once per process; its
// plaintext is irrelevant because the verification is only ever expected to
// fail.
func equalizeLoginTiming(candidate string) {
	timingDummyHashOnce.Do(func() {
		timingDummyHash, _ = users.HashPassword("kypost-timing-equalization-dummy")
	})
	users.VerifySecretHash(timingDummyHash, candidate)
}
