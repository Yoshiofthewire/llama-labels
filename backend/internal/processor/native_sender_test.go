package processor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestNewRelaySenderFromEnvRequiresKey(t *testing.T) {
	t.Setenv("PUSH_RELAY_URL", "https://relay.example")
	t.Setenv("PUSH_RELAY_KEY", "")
	if sender := newRelaySenderFromEnv(nil); sender != nil {
		t.Fatal("newRelaySenderFromEnv should return nil when PUSH_RELAY_KEY is empty")
	}

	t.Setenv("PUSH_RELAY_URL", "")
	t.Setenv("PUSH_RELAY_KEY", "test-api-key")
	if sender := newRelaySenderFromEnv(nil); sender != nil {
		t.Fatal("newRelaySenderFromEnv should return nil when PUSH_RELAY_URL is empty")
	}
}
