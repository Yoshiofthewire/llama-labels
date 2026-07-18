package llama

import "testing"

func TestResetWarmupStateClearsReadiness(t *testing.T) {
	const key = "test-warmup-key"

	state := getWarmupState(key)
	state.mu.Lock()
	state.ready = true
	state.mu.Unlock()

	if got := getWarmupState(key); !got.ready {
		t.Fatal("test setup invalid: expected warmup state to be marked ready before reset")
	}

	ResetWarmupState()

	if got := getWarmupState(key); got.ready {
		t.Fatal("expected ResetWarmupState to clear cached warmup readiness")
	}
}
