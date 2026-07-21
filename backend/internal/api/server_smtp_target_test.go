package api

import "testing"

// TestResolveSMTPTarget covers the fallback chain shared by every
// outbound-send call site: payload.SMTPHost, then SMTP_HOST env var, then
// deriveSMTPHost(payload.Host), then a hardcoded default port of 587 (or an
// error when no host can be determined at all).
func TestResolveSMTPTarget(t *testing.T) {
	t.Run("uses payload SMTPHost directly", func(t *testing.T) {
		payload := imapConfigPayload{
			Host:     "imap.example.com",
			SMTPHost: "smtp.explicit.example.com",
			SMTPPort: 2525,
		}
		host, port, addr, err := resolveSMTPTarget(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if host != "smtp.explicit.example.com" {
			t.Errorf("host = %q, want smtp.explicit.example.com", host)
		}
		if port != 2525 {
			t.Errorf("port = %d, want 2525", port)
		}
		if addr != "smtp.explicit.example.com:2525" {
			t.Errorf("addr = %q, want smtp.explicit.example.com:2525", addr)
		}
	})

	t.Run("falls back to SMTP_HOST env var when payload host empty", func(t *testing.T) {
		t.Setenv("SMTP_HOST", "smtp.fromenv.example.com")
		payload := imapConfigPayload{
			Host: "imap.example.com",
		}
		host, port, addr, err := resolveSMTPTarget(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if host != "smtp.fromenv.example.com" {
			t.Errorf("host = %q, want smtp.fromenv.example.com", host)
		}
		if port != 587 {
			t.Errorf("port = %d, want default 587", port)
		}
		if addr != "smtp.fromenv.example.com:587" {
			t.Errorf("addr = %q, want smtp.fromenv.example.com:587", addr)
		}
	})

	t.Run("falls back to deriveSMTPHost(payload.Host) when payload and env both empty", func(t *testing.T) {
		payload := imapConfigPayload{
			Host: "imap.example.com",
		}
		host, port, addr, err := resolveSMTPTarget(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := deriveSMTPHost("imap.example.com")
		if host != want {
			t.Errorf("host = %q, want %q (from deriveSMTPHost)", host, want)
		}
		if port != 587 {
			t.Errorf("port = %d, want default 587", port)
		}
		if addr != want+":587" {
			t.Errorf("addr = %q, want %s:587", addr, want)
		}
	})

	t.Run("errors when completely unconfigured", func(t *testing.T) {
		payload := imapConfigPayload{
			Host: "",
		}
		_, _, _, err := resolveSMTPTarget(payload)
		if err == nil {
			t.Fatal("expected error when no smtp host can be determined, got nil")
		}
	})
}
