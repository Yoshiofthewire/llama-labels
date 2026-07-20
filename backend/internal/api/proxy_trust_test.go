package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func forwardedRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/health", nil)
	req.RemoteAddr = "203.0.113.50:40000"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-For", "10.0.0.99")
	return req
}

// Default behavior (flag unset): the container is assumed to sit behind a
// TLS-terminating reverse proxy that sets X-Forwarded-*, so those headers are
// trusted. This test pins the default so adding the flag doesn't break every
// existing deployment.
func TestProxyHeadersTrustedByDefault(t *testing.T) {
	req := forwardedRequest(t)
	if !isRequestSecure(req) {
		t.Fatal("default: X-Forwarded-Proto=https should mark the request secure")
	}
	if got := externalBaseURL(req); got != "https://attacker.example" {
		t.Fatalf("default: externalBaseURL = %q, want the forwarded host honored", got)
	}
	if got := clientIP(req); got != "10.0.0.99" {
		t.Fatalf("default: clientIP = %q, want the forwarded address", got)
	}
}

// clientIP must use the RIGHT-most X-Forwarded-For hop (the address the
// nearest trusted proxy appended), not the left-most one which a client can
// prepend. An appending proxy turns a client-sent "1.1.1.1" into
// "1.1.1.1, <realip>"; keying anything (e.g. the login lockout) on the
// left-most hop lets a client rotate it freely.
func TestClientIPUsesRightmostForwardedHop(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.77")
	if got := clientIP(req); got != "203.0.113.77" {
		t.Fatalf("clientIP = %q, want the right-most hop 203.0.113.77 (not the client-prepended 1.1.1.1)", got)
	}
}

// TRUST_PROXY_HEADERS=false: a directly-exposed deployment must not let
// clients influence scheme/host/IP decisions by sending forged X-Forwarded-*
// headers.
func TestProxyHeadersIgnoredWhenTrustDisabled(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "false")
	req := forwardedRequest(t)
	if isRequestSecure(req) {
		t.Fatal("trust disabled: a forged X-Forwarded-Proto must not mark a plain-HTTP request secure")
	}
	if got := externalBaseURL(req); got != "http://backend.internal" {
		t.Fatalf("trust disabled: externalBaseURL = %q, want the connection's own host", got)
	}
	if got := clientIP(req); got != "203.0.113.50" {
		t.Fatalf("trust disabled: clientIP = %q, want the connection's own address", got)
	}
}
