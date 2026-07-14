package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llama-lab/backend/internal/contacts"
	"llama-lab/backend/internal/pgpmail"
)

func TestDecodeMailRequestParsesEncryptAndSign(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"to":      "bob@example.com",
		"subject": "hi",
		"body":    "hello",
		"encrypt": true,
		"sign":    true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mail/send", bytes.NewReader(body))
	decoded, errMsg, err := decodeMailRequest(req)
	if err != nil {
		t.Fatalf("decodeMailRequest: %v (%s)", err, errMsg)
	}
	if !decoded.Encrypt || !decoded.Sign {
		t.Fatalf("expected Encrypt and Sign both true, got %+v", decoded)
	}
}

func TestFindContactPGPKey(t *testing.T) {
	store, err := contacts.New(t.TempDir())
	if err != nil {
		t.Fatalf("contacts.New: %v", err)
	}
	if _, err := store.Upsert(contacts.Contact{
		FormattedName: "Bob",
		Emails:        []contacts.ContactValue{{Value: "Bob@Example.com"}},
		PGPKey:        "-----BEGIN PGP PUBLIC KEY BLOCK-----\n...\n-----END PGP PUBLIC KEY BLOCK-----",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	key, ok := findContactPGPKey(store, "bob@example.com")
	if !ok || key == "" {
		t.Fatalf("expected a key for bob@example.com, got ok=%v key=%q", ok, key)
	}

	if _, ok := findContactPGPKey(store, "nobody@example.com"); ok {
		t.Fatal("expected no key for an unknown address")
	}
}

// TestBuildEncryptedSendArgsKeepsFullRecipientsInSentFolder guards against a
// regression where the encrypted-send branch passed the with-key-only
// filtered lists to finishMailSend's Sent-folder parameters, silently
// dropping pickup-notified (no-key) recipients from the sender's own Sent
// record even though they received a plaintext notification. The Sent
// record must list every original recipient; only the SMTP envelope should
// be restricted to the with-key subset.
func TestBuildEncryptedSendArgsKeepsFullRecipientsInSentFolder(t *testing.T) {
	toList := []string{"alice@example.com", "bob@example.com"}
	ccList := []string{"carol@example.com"}
	bccList := []string{"dave@example.com"}
	withKeyEmails := []string{"alice@example.com", "carol@example.com"} // bob and dave have no key

	draftTo, draftCC, draftBCC, smtpRecipients := buildEncryptedSendArgs(toList, ccList, bccList, withKeyEmails)

	// Sent-folder record: must retain every original recipient, including
	// the no-key ones who only got a pickup notification.
	if len(draftTo) != 2 || draftTo[0] != "alice@example.com" || draftTo[1] != "bob@example.com" {
		t.Fatalf("draftTo should equal original toList unfiltered, got %v", draftTo)
	}
	if len(draftCC) != 1 || draftCC[0] != "carol@example.com" {
		t.Fatalf("draftCC should equal original ccList unfiltered, got %v", draftCC)
	}
	if len(draftBCC) != 1 || draftBCC[0] != "dave@example.com" {
		t.Fatalf("draftBCC should equal original bccList unfiltered, got %v", draftBCC)
	}

	// SMTP envelope: must be restricted to the with-key subset only — the
	// encrypted bytes must never be sent to a recipient without a key.
	wantSMTP := []string{"alice@example.com", "carol@example.com"}
	if len(smtpRecipients) != len(wantSMTP) {
		t.Fatalf("smtpRecipients length mismatch: got %v want %v", smtpRecipients, wantSMTP)
	}
	for i := range wantSMTP {
		if smtpRecipients[i] != wantSMTP[i] {
			t.Fatalf("smtpRecipients mismatch at %d: got %v want %v", i, smtpRecipients, wantSMTP)
		}
	}
	// bob and dave (no key) must not appear in the SMTP envelope.
	for _, r := range smtpRecipients {
		if r == "bob@example.com" || r == "dave@example.com" {
			t.Fatalf("smtpRecipients must not include no-key recipient %q, got %v", r, smtpRecipients)
		}
	}
}

// TestEncryptSignerOnlyPassesIdentityWhenSignRequested guards against the
// encrypt-implicitly-signs regression: handleMailSend eagerly loads a signer
// identity whenever req.Sign || req.Encrypt is true (so it can also cover
// the sign-only branch and the "signing requires an identity" 400 check),
// but that eagerly loaded identity must never leak into EncryptMIME's signer
// argument unless the caller explicitly asked to sign. Encrypt and Sign are
// independent per-email toggles.
func TestEncryptSignerOnlyPassesIdentityWhenSignRequested(t *testing.T) {
	identity, err := pgpmail.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	if got := encryptSigner(identity, false); got != nil {
		t.Fatalf("Encrypt=true,Sign=false: expected nil signer even though an identity exists, got %+v", got)
	}
	if got := encryptSigner(identity, true); got != identity {
		t.Fatalf("Encrypt=true,Sign=true: expected the loaded identity to be passed through")
	}
	if got := encryptSigner(nil, true); got != nil {
		t.Fatalf("expected nil to stay nil when no identity was loaded, got %+v", got)
	}
	if got := encryptSigner(nil, false); got != nil {
		t.Fatalf("expected nil to stay nil when no identity was loaded, got %+v", got)
	}
}

func TestIntersectPreservesOrderAndIsCaseInsensitive(t *testing.T) {
	got := intersect(
		[]string{"Alice@Example.com", "bob@example.com", "carol@example.com"},
		[]string{"bob@example.com", "ALICE@EXAMPLE.COM"},
	)
	want := []string{"Alice@Example.com", "bob@example.com"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v want %v", i, got, want)
		}
	}
}
