package api

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is the app-wide CSP. It is the second line of defense
// for the single riskiest thing this app does — rendering sender-controlled
// HTML email — so a future DOMPurify bypass lands on a page that still can't
// run injected script. Every allowance is tied to a concrete feature:
//
//   - challenges.cloudflare.com (script + frame): the Turnstile login CAPTCHA
//   - cdn.jsdelivr.net (script + connect) and 'wasm-unsafe-eval' + blob:
//     workers: the Friendly Captcha widget and its WASM proof-of-work
//   - fonts.googleapis.com / fonts.gstatic.com: the fonts index.html loads
//   - style-src 'unsafe-inline': inline style attributes in sanitized email
//     HTML and the Quill compose editor
//   - img-src/media-src https: http: data:: remote email content, shown only
//     after the user opts in per message (frontend blocks images by default)
//
// Notably absent: 'unsafe-inline'/'unsafe-eval' for scripts, and any
// wildcard script or connect source.
var contentSecurityPolicy = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com https://cdn.jsdelivr.net",
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
	"img-src 'self' data: https: http:",
	"media-src 'self' data: https: http:",
	"font-src 'self' data: https://fonts.gstatic.com",
	"connect-src 'self' https://cdn.jsdelivr.net",
	"frame-src https://challenges.cloudflare.com",
	"worker-src 'self' blob:",
	"object-src 'none'",
	"frame-ancestors 'none'",
	"base-uri 'self'",
	"form-action 'self'",
}, "; ")

// withSecurityHeaders stamps defense-in-depth headers on every response the
// server produces — API JSON, frontend assets, attachment downloads, the
// CardDAV surface, and the unauthenticated pickup page alike. HSTS is only
// meaningful (and only safe to assert) once the request demonstrably arrived
// over TLS, so it keys off isRequestSecure rather than being unconditional.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if isRequestSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
