package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersOnAllResponses(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	for _, directive := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing directive %q; got %q", directive, csp)
		}
	}
	// The login CAPTCHA widgets and Google Fonts are the only third-party
	// origins the SPA legitimately loads from; the email read view needs
	// remote images once the user opts in.
	for _, allowance := range []string{
		"https://challenges.cloudflare.com",
		"https://fonts.googleapis.com",
		"https://fonts.gstatic.com",
		"https://cdn.jsdelivr.net",
		"img-src 'self' data: https: http:",
	} {
		if !strings.Contains(csp, allowance) {
			t.Errorf("CSP missing required allowance %q; got %q", allowance, csp)
		}
	}

	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("plain-HTTP response must not carry HSTS, got %q", got)
	}
}

func TestHSTSOnSecureRequestsOnly(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	srv.routes().ServeHTTP(rec, req)

	if got := rec.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=") {
		t.Fatalf("Strict-Transport-Security = %q, want a max-age directive on a TLS-terminated request", got)
	}
}
