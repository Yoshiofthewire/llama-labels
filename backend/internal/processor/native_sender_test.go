package processor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"llama-lab/backend/internal/state"
)

func TestRelaySenderSendSuccess(t *testing.T) {
	var seenAuth string
	var seenBody relaySendRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "test-api-key")

	sender := newRelaySenderFromEnv(nil)
	if sender == nil {
		t.Fatal("newRelaySenderFromEnv(nil) returned nil")
	}
	sender.client = ts.Client()

	err := sender.Send(context.Background(), state.NativeDevice{PushToken: "device-token", Platform: "android"}, NativePushMessage{Title: "Title", Body: "Body", Data: map[string]string{"messageId": "m1"}})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if seenAuth != "Bearer test-api-key" {
		t.Fatalf("authorization header = %q, want %q", seenAuth, "Bearer test-api-key")
	}
	if seenBody.Token != "device-token" || seenBody.Title != "Title" || seenBody.Body != "Body" {
		t.Fatalf("unexpected relay body: %+v", seenBody)
	}
	if seenBody.Platform != "android" {
		t.Fatalf("platform = %q, want %q", seenBody.Platform, "android")
	}
	if seenBody.Data["messageId"] != "m1" {
		t.Fatalf("data messageId = %q, want %q", seenBody.Data["messageId"], "m1")
	}
}

func TestRelaySenderSendReturnsStaleError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"stale":true}`))
	}))
	defer ts.Close()

	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "test-api-key")

	sender := newRelaySenderFromEnv(nil)
	if sender == nil {
		t.Fatal("newRelaySenderFromEnv(nil) returned nil")
	}
	sender.client = ts.Client()

	err := sender.Send(context.Background(), state.NativeDevice{PushToken: "device-token"}, NativePushMessage{Title: "Title", Body: "Body"})
	if !errors.Is(err, ErrNativeDeviceStale) {
		t.Fatalf("Send() error = %v, want ErrNativeDeviceStale", err)
	}
}

func TestNewRelaySenderFromEnvRequiresURL(t *testing.T) {
	t.Setenv("PUSH_RELAY_URL", "")
	t.Setenv("PUSH_RELAY_KEY", "test-api-key")
	if sender := newRelaySenderFromEnv(nil); sender != nil {
		t.Fatal("newRelaySenderFromEnv should return nil when PUSH_RELAY_URL is empty")
	}
}

// When there is no key and registration fails (e.g. the relay has registration
// disabled), the sender must not be created.
func TestNewRelaySenderFromEnvRegistrationFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"self-registration is disabled"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "")
	t.Setenv("PUSH_RELAY_KEY_FILE", filepath.Join(t.TempDir(), "push_relay_key"))

	if sender := newRelaySenderFromEnv(nil); sender != nil {
		t.Fatal("newRelaySenderFromEnv should return nil when registration fails")
	}
}

// With no key configured, the server self-registers, persists the key, reuses it
// on the next boot, and does not register a second time.
func TestNewRelaySenderFromEnvAutoRegisterPersistsAndReuses(t *testing.T) {
	var registerCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&registerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id-1","label":"srv","key":"minted-key","expiresAt":null}`))
	}))
	defer ts.Close()

	keyFile := filepath.Join(t.TempDir(), "push_relay_key")
	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "")
	t.Setenv("PUSH_RELAY_KEY_FILE", keyFile)

	sender := newRelaySenderFromEnv(nil)
	if sender == nil {
		t.Fatal("expected sender from auto-registration")
	}
	if sender.apiKey != "minted-key" {
		t.Fatalf("apiKey = %q, want %q", sender.apiKey, "minted-key")
	}
	if got := atomic.LoadInt32(&registerCalls); got != 1 {
		t.Fatalf("register calls = %d, want 1", got)
	}
	if b, err := os.ReadFile(keyFile); err != nil || string(b) != "minted-key\n" {
		t.Fatalf("key file = %q (err %v), want %q", string(b), err, "minted-key\n")
	}

	// Second boot: key is read from the file, no re-registration.
	sender2 := newRelaySenderFromEnv(nil)
	if sender2 == nil || sender2.apiKey != "minted-key" {
		t.Fatalf("second sender did not reuse persisted key: %+v", sender2)
	}
	if got := atomic.LoadInt32(&registerCalls); got != 1 {
		t.Fatalf("register calls after reuse = %d, want 1 (should not re-register)", got)
	}
}

// An explicit PUSH_RELAY_KEY takes precedence and never triggers registration or
// touches the key file.
func TestNewRelaySenderFromEnvExplicitKeyWins(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("registration must not be called when PUSH_RELAY_KEY is set (path %s)", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	keyFile := filepath.Join(t.TempDir(), "push_relay_key")
	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "explicit-key")
	t.Setenv("PUSH_RELAY_KEY_FILE", keyFile)

	sender := newRelaySenderFromEnv(nil)
	if sender == nil || sender.apiKey != "explicit-key" {
		t.Fatalf("expected explicit key to win, got %+v", sender)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key file should not be written when PUSH_RELAY_KEY is set (stat err = %v)", err)
	}
}
