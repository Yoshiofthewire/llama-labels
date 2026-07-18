package imap

import (
	"testing"

	goimap "github.com/BrianLeishman/go-imap"
)

func TestPgpDetectSignature(t *testing.T) {
	t.Run("no attachments", func(t *testing.T) {
		if sig := pgpDetectSignature(nil); sig != "" {
			t.Fatalf("expected empty signature, got %q", sig)
		}
	})

	t.Run("no signature attachment", func(t *testing.T) {
		attachments := []goimap.Attachment{
			{Name: "photo.png", MimeType: "image/png", Content: []byte{0x89, 0x50, 0x4e, 0x47}},
		}
		if sig := pgpDetectSignature(attachments); sig != "" {
			t.Fatalf("expected empty signature, got %q", sig)
		}
	})

	t.Run("armored signature attachment", func(t *testing.T) {
		armored := "-----BEGIN PGP SIGNATURE-----\n\nfakebase64data\n-----END PGP SIGNATURE-----\n"
		attachments := []goimap.Attachment{
			{Name: "unrelated.txt", MimeType: "text/plain", Content: []byte("hello")},
			{Name: "signature.asc", MimeType: "application/pgp-signature", Content: []byte(armored)},
		}
		got := pgpDetectSignature(attachments)
		if got != armored {
			t.Fatalf("expected %q, got %q", armored, got)
		}
	})

	t.Run("leading whitespace still detected", func(t *testing.T) {
		armored := "  \n-----BEGIN PGP SIGNATURE-----\n\nfakebase64data\n-----END PGP SIGNATURE-----\n"
		attachments := []goimap.Attachment{
			{Name: "signature.asc", MimeType: "application/pgp-signature", Content: []byte(armored)},
		}
		if got := pgpDetectSignature(attachments); got != armored {
			t.Fatalf("expected %q, got %q", armored, got)
		}
	})
}
