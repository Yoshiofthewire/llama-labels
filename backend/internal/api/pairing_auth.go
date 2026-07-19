package api

import (
	"net/http"
	"strings"
)

// Headers a paired native client (Android/Linux/Mac) sends its pairing
// credentials in. Preferred over the legacy ?sub=&hash= query params, which
// stay supported as a fallback for clients that haven't updated yet — see
// docs/superpowers/plans/2026-07-19-pairing-auth-headers.md.
const (
	headerSubscriberID   = "X-Kypost-Subscriber-Id"
	headerSubscriberHash = "X-Kypost-Subscriber-Hash"
)

// pairingCredentialsFromRequest extracts the subscriberId/subscriberHash a
// paired native client presents to prove device pairing. Header values take
// precedence; if the subscriber-id header is absent, the ?sub=&hash= query
// params are read instead. subscriberHash is lowercased to match the hex
// pairingSubscriberHash() produces.
func pairingCredentialsFromRequest(r *http.Request) (subscriberID, subscriberHash string) {
	if id := strings.TrimSpace(r.Header.Get(headerSubscriberID)); id != "" {
		return id, strings.ToLower(strings.TrimSpace(r.Header.Get(headerSubscriberHash)))
	}
	return strings.TrimSpace(r.URL.Query().Get("sub")),
		strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hash")))
}
