package api

import (
	"testing"
	"time"
)

// A login attempt for a nonexistent username must pay comparable scrypt cost
// to a wrong-password attempt against a real account: if the unknown-user
// path returns without hashing, response timing tells an attacker which
// usernames exist.
func TestLoginTimingDoesNotRevealUnknownUsernames(t *testing.T) {
	srv := newTestServer(t)
	all, err := srv.users.List()
	if err != nil || len(all) == 0 {
		t.Fatalf("no test user available: %v", err)
	}

	start := time.Now()
	loginAttempt(srv, all[0].Username, "wrong-password", "203.0.113.40:40000")
	realUserElapsed := time.Since(start)

	start = time.Now()
	loginAttempt(srv, "no-such-user-anywhere", "wrong-password", "203.0.113.40:40000")
	unknownUserElapsed := time.Since(start)

	// Both paths must run scrypt. A generous 4x tolerance absorbs scheduler
	// noise while still failing hard when the unknown-user path skips the
	// hash entirely (microseconds vs tens of milliseconds).
	if unknownUserElapsed < realUserElapsed/4 {
		t.Fatalf("unknown-username login returned too fast: %v vs %v for a real user — timing reveals whether a username exists",
			unknownUserElapsed, realUserElapsed)
	}
}
