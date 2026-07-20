package pgpmail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"kypost-server/backend/internal/mailmsg"
)

// extractOctetStreamPart is a test-only MIME walker that finds the armored
// PGP data part EncryptMIME produces, mirroring the content-sniffing
// technique (crypto.IsPGPMessage) the receive-path integration (Task 7) uses
// against real IMAP-fetched attachments.
func extractOctetStreamPart(t *testing.T, raw []byte) (string, bool) {
	t.Helper()
	_, content, err := splitMessage(raw)
	if err != nil {
		t.Fatalf("splitMessage: %v", err)
	}
	_, attachments, err := ParseContent(content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	for _, a := range attachments {
		if crypto.IsPGPMessage(string(a.Content)) {
			return string(a.Content), true
		}
	}
	return "", false
}

func TestEncryptDecryptMIMERoundTrip(t *testing.T) {
	alice, err := GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	bob, err := GenerateIdentity("Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity bob: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Secret",
		Body:    "meet at dawn",
		Mode:    "plain",
	}.Build()

	encrypted, err := EncryptMIME(plaintext, []string{bob.ArmoredPublicKey}, alice)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	if !strings.Contains(string(encrypted), "multipart/encrypted") {
		t.Fatal("expected multipart/encrypted content type in output")
	}
	if !strings.Contains(string(encrypted), "Subject: Secret") {
		t.Fatal("expected Subject header preserved on the outer envelope")
	}

	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}

	result, err := DecryptMIME(armoredData, bob, []string{alice.ArmoredPublicKey})
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	if !result.Verified {
		t.Fatal("expected signature to verify")
	}
	if result.SignerFingerprint != alice.Fingerprint {
		t.Fatalf("signer fingerprint mismatch: got %s want %s", result.SignerFingerprint, alice.Fingerprint)
	}
	body, attachments, err := ParseContent(result.Content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if body != "meet at dawn" {
		t.Fatalf("body mismatch: got %q", body)
	}
	if len(attachments) != 0 {
		t.Fatalf("expected no attachments, got %d", len(attachments))
	}
}

func TestEncryptDecryptMIMEWithAttachment(t *testing.T) {
	alice, err := GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"alice@example.com"},
		Subject: "With attachment",
		Body:    "see attached",
		Mode:    "plain",
		Attachments: []mailmsg.Attachment{
			{Name: "note.txt", MimeType: "text/plain", Content: []byte("hello file")},
		},
	}.Build()

	encrypted, err := EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}

	result, err := DecryptMIME(armoredData, alice, nil)
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	// EncryptMIME was called with a nil signer above: the resulting
	// ciphertext must not carry an embedded signature. This guards against
	// the encrypt-implicitly-signs regression where a caller's eagerly
	// loaded identity leaked into EncryptMIME's signer argument even when
	// signing wasn't requested.
	if result.Signed {
		t.Fatal("expected unsigned result when EncryptMIME was called with a nil signer")
	}
	body, attachments, err := ParseContent(result.Content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if body != "see attached" {
		t.Fatalf("body mismatch: got %q", body)
	}
	if len(attachments) != 1 || attachments[0].Name != "note.txt" || string(attachments[0].Content) != "hello file" {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
}

func TestSignMIMEAndVerifyDetached(t *testing.T) {
	alice, err := GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Signed only",
		Body:    "trust me",
		Mode:    "plain",
	}.Build()

	signed, err := SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}
	if !strings.Contains(string(signed), "multipart/signed") {
		t.Fatal("expected multipart/signed content type in output")
	}

	_, content, err := splitMessage(plaintext)
	if err != nil {
		t.Fatalf("splitMessage: %v", err)
	}
	sigStart := strings.Index(string(signed), "-----BEGIN PGP SIGNATURE-----")
	if sigStart == -1 {
		t.Fatal("expected an armored signature block in the output")
	}
	sigEnd := strings.Index(string(signed)[sigStart:], "-----END PGP SIGNATURE-----") + len("-----END PGP SIGNATURE-----")
	armoredSig := string(signed)[sigStart : sigStart+sigEnd]

	result, err := VerifyDetached(content, armoredSig, []string{alice.ArmoredPublicKey})
	if err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
	if !result.Verified {
		t.Fatal("expected signature to verify")
	}
	if result.SignerFingerprint != alice.Fingerprint {
		t.Fatalf("signer fingerprint mismatch: got %s want %s", result.SignerFingerprint, alice.Fingerprint)
	}
}

// TestSignMIMEWithAttachmentPreservesTrailingCRLF is a regression test for a
// bug in buildSignedEnvelope: when content (the signed part) already ends in
// its own "\r\n" — which mailmsg.Message.Build() always produces for
// multipart/mixed messages, since mime/multipart.Writer.Close() terminates
// with "\r\n--boundary--\r\n" — the buggy code skipped writing the boundary
// delimiter's own CRLF, silently truncating 2 bytes off the signed content
// as understood by any real MIME parser (which always strips exactly one
// CRLF as the delimiter separator, not two). That corruption doesn't show up
// by inspecting the produced bytes directly; it only appears once the
// envelope is parsed back through a real mime/multipart.Reader, so this test
// does exactly that instead of just calling VerifyDetached in-process.
func TestSignMIMEWithAttachmentPreservesTrailingCRLF(t *testing.T) {
	alice, err := GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Signed with attachment",
		Body:    "see attached",
		Mode:    "plain",
		Attachments: []mailmsg.Attachment{
			{Name: "note.txt", MimeType: "text/plain", Content: []byte("hello file")},
		},
	}.Build()

	_, wantContent, err := splitMessage(plaintext)
	if err != nil {
		t.Fatalf("splitMessage: %v", err)
	}
	if !bytes.HasSuffix(wantContent, []byte("\r\n")) {
		t.Fatalf("test setup invalid: expected signed content to end in its own CRLF (multipart/mixed with attachment), got %q", wantContent[len(wantContent)-20:])
	}

	signed, err := SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}

	// Parse the produced envelope's actual wire bytes back through a real
	// net/mail + mime/multipart reader, the same way a real interoperating
	// PGP/MIME client would.
	msg, err := mail.ReadMessage(bytes.NewReader(signed))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("mime.ParseMediaType: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("expected multipart Content-Type, got %q", mediaType)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("mr.NextPart (signed content part): %v", err)
	}

	// NextPart parses content's own embedded "Content-Type: ...\r\n\r\n"
	// prefix as this part's MIME headers, since MIME doesn't distinguish
	// "embedded" from "real" headers. Reconstruct the full part bytes from
	// the parsed header plus the remaining body, and compare that
	// reconstruction against content — not part's body alone.
	partBody, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("io.ReadAll(part): %v", err)
	}
	var gotContent bytes.Buffer
	gotContent.WriteString("Content-Type: " + part.Header.Get("Content-Type") + "\r\n\r\n")
	gotContent.Write(partBody)

	if !bytes.Equal(gotContent.Bytes(), wantContent) {
		t.Fatalf("signed content part corrupted by round-trip through a real MIME parser:\n got  %q\n want %q", gotContent.Bytes(), wantContent)
	}
}
