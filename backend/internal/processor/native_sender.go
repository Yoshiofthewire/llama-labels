package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"llama-lab/backend/internal/fsutil"
	"llama-lab/backend/internal/logging"
	"llama-lab/backend/internal/state"
)

var ErrNativeDeviceStale = errors.New("native device token is stale")

type NativePushMessage struct {
	Title string
	Body  string
	Data  map[string]string
}

// RelaySender forwards native push requests to the central Cloudflare Worker
// relay, which holds the single Firebase service account the published mobile
// app is built against. Self-hosted servers never talk to FCM directly; they
// authenticate to the relay with a per-server API key. The relay delivers to
// every platform (iOS and Android), so it is the only native sender.
type RelaySender struct {
	relayURL string
	apiKey   string
	client   *http.Client
}

// relaySendRequest is the JSON body POSTed to the relay's /send endpoint.
type relaySendRequest struct {
	Token    string            `json:"token"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Data     map[string]string `json:"data,omitempty"`
	Platform string            `json:"platform,omitempty"`
}

func logNativeSenderError(log *logging.Logger, reason, detail string) {
	if log == nil {
		return
	}
	log.Error("native push relay not configured", "reason", reason, "detail", detail)
}

// newRelaySenderFromEnvWithPrefix builds a relay sender for the given prefix
// (e.g. "PUSH_RELAY" or "APNS_RELAY"), or returns nil if not configured.
func newRelaySenderFromEnvWithPrefix(log *logging.Logger, prefix string) *RelaySender {
	relayURL := strings.TrimRight(strings.TrimSpace(os.Getenv(prefix+"_URL")), "/")
	if relayURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}

	apiKey, err := resolveRelayKeyWithPrefix(log, prefix, relayURL, client)
	if err != nil {
		logNativeSenderError(log, prefix+" relay key unavailable", err.Error())
		return nil
	}
	if apiKey == "" {
		logNativeSenderError(log, prefix+"_KEY missing", "no key in "+prefix+"_KEY, the key file, or from auto-registration")
		return nil
	}

	return &RelaySender{
		relayURL: relayURL,
		apiKey:   apiKey,
		client:   client,
	}
}

// relayKeyFilePathWithPrefix is where an auto-registered key is persisted.
// Parameterized by prefix so distinct relays (PUSH_RELAY vs APNS_RELAY) store
// keys in distinct files and don't collide on disk.
func relayKeyFilePathWithPrefix(prefix string) string {
	if p := strings.TrimSpace(os.Getenv(prefix + "_KEY_FILE")); p != "" {
		return p
	}
	dir := strings.TrimSpace(os.Getenv("SECRET_DIR"))
	if dir == "" {
		dir = "/llama_lab/private"
	}
	// e.g. "push_relay_key" for PUSH_RELAY, "apns_relay_key" for APNS_RELAY.
	name := strings.ToLower(strings.TrimSuffix(prefix, "_RELAY")) + "_relay_key"
	return filepath.Join(dir, name)
}

// resolveRelayKeyWithPrefix obtains the per-server relay key for a given relay prefix,
// in order of preference:
//  1. {prefix}_KEY (explicit env, e.g. an operator-issued key)
//  2. the persisted key file (a previous auto-registration)
//  3. auto-registration: POST /register, then persist the returned key
func resolveRelayKeyWithPrefix(log *logging.Logger, prefix, relayURL string, client *http.Client) (string, error) {
	if key := strings.TrimSpace(os.Getenv(prefix + "_KEY")); key != "" {
		return key, nil
	}

	path := relayKeyFilePathWithPrefix(prefix)
	if b, err := os.ReadFile(path); err == nil {
		if key := strings.TrimSpace(string(b)); key != "" {
			return key, nil
		}
	}

	key, err := registerWithRelay(relayURL, client)
	if err != nil {
		return "", err
	}
	if err := fsutil.AtomicWriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		if log != nil {
			log.Error("failed to persist auto-registered relay key", "prefix", prefix, "path", path, "error", err.Error())
		}
	} else if log != nil {
		log.Info("auto-registered with relay", "prefix", prefix, "key_file", path)
	}
	return key, nil
}

// registerWithRelay self-issues a per-server key from the relay's public
// /register endpoint. No credentials are required; the relay ties one active
// key to the requesting IP.
func registerWithRelay(relayURL string, client *http.Client) (string, error) {
	label, _ := os.Hostname()
	if strings.TrimSpace(label) == "" {
		label = "llama-lab"
	}
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("push relay registration failed: status=%d response=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("push relay registration returned invalid JSON: %w", err)
	}
	key := strings.TrimSpace(parsed.Key)
	if key == "" {
		return "", errors.New("push relay registration returned no key")
	}
	return key, nil
}

// NativePushDispatcher routes native push notifications to the appropriate relay
// (FCM for Android, direct APNs for iOS) based on device.Platform.
type NativePushDispatcher struct {
	fcmSender  *RelaySender
	apnsSender *RelaySender
}

// NewNativePushDispatcher constructs a dispatcher with both FCM (PUSH_RELAY_*)
// and APNs (APNS_RELAY_*) senders. Either or both may be nil if not configured.
func NewNativePushDispatcher(log *logging.Logger) *NativePushDispatcher {
	return &NativePushDispatcher{
		fcmSender:  newRelaySenderFromEnvWithPrefix(log, "PUSH_RELAY"),
		apnsSender: newRelaySenderFromEnvWithPrefix(log, "APNS_RELAY"),
	}
}

// senderFor returns the appropriate sender for a given platform. Apple
// platforms (iOS and macOS) share the APNs relay; everything else goes to FCM.
func (d *NativePushDispatcher) senderFor(platform string) *RelaySender {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "ios", "macos":
		return d.apnsSender
	}
	return d.fcmSender
}

// Send dispatches a native push to the appropriate relay based on device.Platform.
func (d *NativePushDispatcher) Send(ctx context.Context, device state.NativeDevice, message NativePushMessage) error {
	sender := d.senderFor(device.Platform)
	if sender == nil {
		platform := strings.TrimSpace(device.Platform)
		if platform == "" {
			platform = "unknown"
		}
		return fmt.Errorf("native push relay not configured for platform %q", platform)
	}
	return sender.Send(ctx, device, message)
}

func (s *RelaySender) Send(ctx context.Context, device state.NativeDevice, message NativePushMessage) error {
	registrationToken := strings.TrimSpace(device.PushToken)
	if registrationToken == "" {
		return errors.New("missing push token")
	}

	body, err := json.Marshal(relaySendRequest{
		Token:    registrationToken,
		Title:    message.Title,
		Body:     message.Body,
		Data:     message.Data,
		Platform: device.Platform,
	})
	if err != nil {
		return err
	}

	sendURL := s.relayURL + "/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	trimmed := strings.TrimSpace(string(respBody))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if isRelayStaleResponse(resp.StatusCode, trimmed) {
		return fmt.Errorf("%w: status=%d response=%s", ErrNativeDeviceStale, resp.StatusCode, trimmed)
	}
	return fmt.Errorf("push relay send failed: status=%d response=%s", resp.StatusCode, trimmed)
}

// isRelayStaleResponse reports whether the relay signalled that the device
// token is no longer registered. The relay returns HTTP 410 with
// {"stale":true} for unregistered tokens; we also match the underlying FCM
// wording defensively in case it is surfaced verbatim.
func isRelayStaleResponse(statusCode int, response string) bool {
	if statusCode == http.StatusGone {
		return true
	}
	lower := strings.ToLower(response)
	if strings.Contains(lower, `"stale":true`) || strings.Contains(lower, `"stale": true`) {
		return true
	}
	if strings.Contains(lower, "unregistered") || strings.Contains(lower, "notregistered") || strings.Contains(lower, "registration-token-not-registered") {
		return true
	}
	if statusCode == http.StatusNotFound && strings.Contains(lower, "requested entity was not found") {
		return true
	}
	return false
}
