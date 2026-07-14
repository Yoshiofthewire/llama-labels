package api

import "testing"

func TestNormalizeNativeTransport(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		platform  string
		want      string
	}{
		// Explicit transports are returned as-is.
		{"fcm explicit", "fcm", "android", "fcm"},
		{"apns explicit", "apns", "android", "apns"},
		{"unifiedpush explicit", "unifiedpush", "android", "unifiedpush"},

		// Empty transport is derived from platform.
		{"empty to apns via ios", "", "ios", "apns"},
		{"empty to apns via macos", "", "macos", "apns"},
		{"empty to fcm via android", "", "android", "fcm"},
		{"empty to fcm via unknown", "", "windows", "fcm"},
		{"empty to unifiedpush via linux", "", "linux", "unifiedpush"},
		{"empty to unifiedpush via linux mixed case", "", "  Linux  ", "unifiedpush"},

		// Case-insensitivity.
		{"FCM uppercase", "FCM", "android", "fcm"},
		{"APNS uppercase", "APNS", "ios", "apns"},
		{"UnifiedPush mixed case", "UnifiedPush", "android", "unifiedpush"},

		// Whitespace handling.
		{"fcm with spaces", "  fcm  ", "android", "fcm"},
		{"ios platform with spaces", "", "  ios  ", "apns"},

		// Case-insensitive platform derivation.
		{"ios mixed case derivation", "", "IOS", "apns"},
		{"MacOS mixed case derivation", "", "MacOS", "apns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeNativeTransport(tt.transport, tt.platform)
			if err != nil {
				t.Fatalf("normalizeNativeTransport(%q, %q) returned unexpected error: %v", tt.transport, tt.platform, err)
			}
			if got != tt.want {
				t.Errorf("normalizeNativeTransport(%q, %q) = %q, want %q", tt.transport, tt.platform, got, tt.want)
			}
		})
	}
}

// Unrecognized, non-empty transport strings must fail loudly instead of
// silently defaulting to fcm — a silent fallback would hide client bugs.
func TestNormalizeNativeTransportRejectsUnknown(t *testing.T) {
	if _, err := normalizeNativeTransport("bogus", "android"); err == nil {
		t.Fatal("normalizeNativeTransport(\"bogus\", \"android\") = nil error, want error")
	}
}
