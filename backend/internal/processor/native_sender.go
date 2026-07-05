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
	"strings"
	"time"

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
	apiKey := strings.TrimSpace(os.Getenv("PUSH_RELAY_KEY"))
	if apiKey == "" {
		logNativeSenderError(log, "PUSH_RELAY_KEY missing", "set PUSH_RELAY_KEY to the per-server API key issued by the relay operator")
		return nil
	}

	return &relaySender{
		relayURL: relayURL,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
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
