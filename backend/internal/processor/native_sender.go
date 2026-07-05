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

type NativeSender interface {
	Name() string
	Supports(platform string) bool
	Send(ctx context.Context, device state.NativeDevice, message NativePushMessage) error
}

// relaySender forwards native push requests to the central Cloudflare Worker
// relay, which holds the single Firebase service account the published mobile
// app is built against. Self-hosted servers never talk to FCM directly; they
// authenticate to the relay with a per-server API key.
type relaySender struct {
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

func NewNativeSendersFromEnv(log *logging.Logger) []NativeSender {
	out := make([]NativeSender, 0, 1)
	if sender := newRelaySenderFromEnv(log); sender != nil {
		out = append(out, sender)
	}
	return out
}

func logNativeSenderError(log *logging.Logger, reason, detail string) {
	if log == nil {
		return
	}
	log.Error("native push relay not configured", "reason", reason, "detail", detail)
}

func newRelaySenderFromEnv(log *logging.Logger) *relaySender {
	relayURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUSH_RELAY_URL")), "/")
	if relayURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}

	apiKey, err := resolveRelayKey(log, relayURL, client)
	if err != nil {
		logNativeSenderError(log, "relay key unavailable", err.Error())
		return nil
	}
	if apiKey == "" {
		logNativeSenderError(log, "PUSH_RELAY_KEY missing", "no key in PUSH_RELAY_KEY, the key file, or from auto-registration")
		return nil
	}

	return &relaySender{
		relayURL: relayURL,
		apiKey:   apiKey,
		client:   client,
	}
}

// relayKeyFilePath is where an auto-registered key is persisted so it survives
// restarts (re-registering would mint a new key and invalidate this one, since
// the relay allows one key per IP). Override with PUSH_RELAY_KEY_FILE.
func relayKeyFilePath() string {
	if p := strings.TrimSpace(os.Getenv("PUSH_RELAY_KEY_FILE")); p != "" {
		return p
	}
	dir := strings.TrimSpace(os.Getenv("SECRET_DIR"))
	if dir == "" {
		dir = "/llama_lab/private"
	}
	return filepath.Join(dir, "push_relay_key")
}

// resolveRelayKey obtains the per-server relay key, in order of preference:
//  1. PUSH_RELAY_KEY (explicit env, e.g. an operator-issued key) — never touches
//     the key file.
//  2. the persisted key file (a previous auto-registration).
//  3. auto-registration: POST /register, then persist the returned key so it is
//     reused on the next boot (zero setup for the self-hoster).
func resolveRelayKey(log *logging.Logger, relayURL string, client *http.Client) (string, error) {
	if key := strings.TrimSpace(os.Getenv("PUSH_RELAY_KEY")); key != "" {
		return key, nil
	}

	path := relayKeyFilePath()
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
		// Usable this run, but not persisted — we'll re-register (and supersede
		// this key) next boot. Warn so the operator can fix the volume/path.
		if log != nil {
			log.Error("failed to persist auto-registered push relay key", "path", path, "error", err.Error())
		}
	} else if log != nil {
		log.Info("auto-registered with push relay", "key_file", path)
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

func (s *relaySender) Name() string {
	return "relay"
}

func (s *relaySender) Supports(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "android", "ios":
		return true
	default:
		return false
	}
}

func (s *relaySender) Send(ctx context.Context, device state.NativeDevice, message NativePushMessage) error {
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

func SelectNativeSender(senders []NativeSender, platform string) NativeSender {
	for _, sender := range senders {
		if sender.Supports(platform) {
			return sender
		}
	}
	return nil
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
