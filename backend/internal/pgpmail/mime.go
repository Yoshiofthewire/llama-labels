package pgpmail

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"kypost-server/backend/internal/mailmsg"
)

// envelopeHeaderOrder lists the outer RFC 5322 headers preserved verbatim on
// a PGP/MIME-wrapped message, in the order mailmsg.Message.Build() writes
// them. PGP/MIME wraps only the *content* (Content-Type + body); routing and
// display headers stay outside the encrypted/signed part — OpenPGP does not
// protect message routing metadata, only the body.
var envelopeHeaderOrder = []string{"From", "To", "Cc", "Bcc", "Subject"}

// splitMessage separates a raw RFC 5322 message (as produced by
// mailmsg.Message.Build()) into its preserved envelope headers and its inner
// content part (the original Content-Type header plus body, byte-identical
// to the input).
func splitMessage(raw []byte) (envelope textproto.MIMEHeader, content []byte, err error) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && header == nil {
		return nil, nil, fmt.Errorf("pgpmail: read message headers: %w", err)
	}

	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain; charset=UTF-8"
	}

	var body bytes.Buffer
	if _, err := body.ReadFrom(reader.R); err != nil {
		return nil, nil, fmt.Errorf("pgpmail: read message body: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString("Content-Type: " + contentType + "\r\n\r\n")
	buf.Write(body.Bytes())

	return header, buf.Bytes(), nil
}

func writeEnvelopeHeaders(w io.Writer, envelope textproto.MIMEHeader) {
	for _, name := range envelopeHeaderOrder {
		if v := envelope.Get(name); v != "" {
			_, _ = io.WriteString(w, name+": "+v+"\r\n")
		}
	}
}

// EncryptMIME wraps a plaintext RFC 5322 message (as produced by
// mailmsg.Message.Build()) in an RFC 3156 multipart/encrypted envelope. The
// message's Content-Type and body become the encrypted payload; From/To/Cc/
// Bcc/Subject stay on the outer, unencrypted envelope headers. If signer is
// non-nil, the content is signed before encryption (combined sign+encrypt,
// verified in one step by DecryptMIME on the way back).
func EncryptMIME(plaintext []byte, recipientArmoredPubKeys []string, signer *Identity) ([]byte, error) {
	if len(recipientArmoredPubKeys) == 0 {
		return nil, errors.New("pgpmail: at least one recipient key required")
	}
	envelope, content, err := splitMessage(plaintext)
	if err != nil {
		return nil, err
	}

	recipients, err := crypto.NewKeyRing(nil)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: new recipient keyring: %w", err)
	}
	for _, armored := range recipientArmoredPubKeys {
		key, err := crypto.NewKeyFromArmored(armored)
		if err != nil {
			return nil, fmt.Errorf("pgpmail: parse recipient key: %w", err)
		}
		if err := recipients.AddKey(key); err != nil {
			return nil, fmt.Errorf("pgpmail: add recipient key: %w", err)
		}
	}

	builder := crypto.PGP().Encryption().Recipients(recipients)
	if signer != nil {
		builder = builder.SigningKey(signer.key)
	}
	encHandle, err := builder.New()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build encryption handle: %w", err)
	}
	pgpMessage, err := encHandle.Encrypt(content)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: encrypt: %w", err)
	}
	armored, err := pgpMessage.Armor()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: armor encrypted message: %w", err)
	}

	return buildEncryptedEnvelope(envelope, armored), nil
}

func buildEncryptedEnvelope(envelope textproto.MIMEHeader, armoredEncrypted string) []byte {
	var msg bytes.Buffer
	writeEnvelopeHeaders(&msg, envelope)
	msg.WriteString("MIME-Version: 1.0\r\n")

	w := multipart.NewWriter(&msg)
	msg.WriteString(`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary=` + w.Boundary() + "\r\n")
	msg.WriteString("\r\n")

	control, _ := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/pgp-encrypted"}})
	_, _ = io.WriteString(control, "Version: 1\r\n")

	data, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {`application/octet-stream; name="encrypted.asc"`},
		"Content-Disposition": {`inline; filename="encrypted.asc"`},
	})
	_, _ = io.WriteString(data, armoredEncrypted)

	_ = w.Close()
	return msg.Bytes()
}

// DecryptResult is the outcome of decrypting a PGP/MIME payload: the
// recovered content (a Content-Type header plus body, ready for
// ParseContent) and, when the message was combined sign+encrypt, the
// signature verification outcome.
type DecryptResult struct {
	Content           []byte
	Signed            bool
	Verified          bool
	SignerFingerprint string
}

// DecryptMIME decrypts an armored OpenPGP message (the data part of a
// multipart/encrypted structure) using recipient's private key. If
// signerArmoredPubKeys is non-empty, an inline signature (present when the
// sender combined sign+encrypt) is verified against them.
func DecryptMIME(armoredPGPMessage string, recipient *Identity, signerArmoredPubKeys []string) (*DecryptResult, error) {
	if recipient == nil {
		return nil, errors.New("pgpmail: recipient identity required to decrypt")
	}
	decryptionKeys, err := crypto.NewKeyRing(recipient.key)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build decryption keyring: %w", err)
	}

	builder := crypto.PGP().Decryption().DecryptionKeys(decryptionKeys)
	var verifying bool
	if len(signerArmoredPubKeys) > 0 {
		verifyKeys, err := crypto.NewKeyRing(nil)
		if err != nil {
			return nil, fmt.Errorf("pgpmail: build verification keyring: %w", err)
		}
		for _, armored := range signerArmoredPubKeys {
			key, err := crypto.NewKeyFromArmored(armored)
			if err != nil {
				return nil, fmt.Errorf("pgpmail: parse signer key: %w", err)
			}
			if err := verifyKeys.AddKey(key); err != nil {
				return nil, fmt.Errorf("pgpmail: add signer key: %w", err)
			}
		}
		builder = builder.VerificationKeys(verifyKeys)
		verifying = true
	}

	decHandle, err := builder.New()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build decryption handle: %w", err)
	}
	result, err := decHandle.Decrypt([]byte(armoredPGPMessage), crypto.Auto)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: decrypt: %w", err)
	}

	out := &DecryptResult{Content: result.Bytes()}
	if verifying {
		out.Signed = len(result.Signatures) > 0
		out.Verified = out.Signed && result.SignatureError() == nil
		if key := result.SignedByKey(); key != nil {
			out.SignerFingerprint = key.GetFingerprint()
		}
	}
	return out, nil
}

// SignMIME wraps a plaintext RFC 5322 message in an RFC 3156 multipart/
// signed envelope: the message's Content-Type and body, byte-identical,
// alongside a detached OpenPGP signature over those exact bytes.
func SignMIME(plaintext []byte, signer *Identity) ([]byte, error) {
	if signer == nil {
		return nil, errors.New("pgpmail: signer identity required")
	}
	envelope, content, err := splitMessage(plaintext)
	if err != nil {
		return nil, err
	}

	signHandle, err := crypto.PGP().Sign().SigningKey(signer.key).Detached().New()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build sign handle: %w", err)
	}
	signature, err := signHandle.Sign(content, crypto.Armor)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: sign: %w", err)
	}

	return buildSignedEnvelope(envelope, content, string(signature)), nil
}

// buildSignedEnvelope hand-assembles the multipart/signed structure instead
// of using mime/multipart.Writer for the first part: stdlib's CreatePart
// always inserts its own blank-line header separator, which would corrupt
// the signed part's bytes (content already carries its own embedded
// Content-Type header line) — the signed part's bytes on the wire must be
// byte-identical to what was passed to Sign, or verification on the
// receiving end fails. Per RFC 2046, the CRLF immediately before a boundary
// delimiter belongs to the delimiter, not the part body — it must always be
// written after content, unconditionally, even when content already ends in
// its own "\r\n" (e.g. a multipart/mixed inner structure with attachments):
// that trailing CRLF is signed data, and a compliant parser will strip
// exactly one additional CRLF as the delimiter separator, not two.
func buildSignedEnvelope(envelope textproto.MIMEHeader, content []byte, armoredSignature string) []byte {
	boundary := randomBoundary()

	var msg bytes.Buffer
	writeEnvelopeHeaders(&msg, envelope)
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString(`Content-Type: multipart/signed; protocol="application/pgp-signature"; micalg="pgp-sha256"; boundary="` + boundary + "\"\r\n")
	msg.WriteString("\r\n")

	msg.WriteString("--" + boundary + "\r\n")
	msg.Write(content)
	msg.WriteString("\r\n")

	msg.WriteString("--" + boundary + "\r\n")
	msg.WriteString("Content-Type: application/pgp-signature; name=\"signature.asc\"\r\n")
	msg.WriteString("Content-Disposition: attachment; filename=\"signature.asc\"\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(armoredSignature)
	msg.WriteString("\r\n")

	msg.WriteString("--" + boundary + "--\r\n")
	return msg.Bytes()
}

func randomBoundary() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("pgpmail-%x", buf)
}

// VerifyResult is the outcome of verifying a standalone detached signature.
type VerifyResult struct {
	Verified          bool
	SignerFingerprint string
}

// VerifyDetached verifies an armored detached signature over data using the
// given signer public keys. Used for best-effort verification of standalone
// (non-encrypted) signed mail fetched via IMAP — see the receive-path task
// and the plan's Global Constraints for why this is best-effort rather than
// exact for third-party mail.
func VerifyDetached(data []byte, armoredSignature string, signerArmoredPubKeys []string) (*VerifyResult, error) {
	if len(signerArmoredPubKeys) == 0 {
		return nil, errors.New("pgpmail: at least one signer key required")
	}
	verifyKeys, err := crypto.NewKeyRing(nil)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build verification keyring: %w", err)
	}
	for _, armored := range signerArmoredPubKeys {
		key, err := crypto.NewKeyFromArmored(armored)
		if err != nil {
			return nil, fmt.Errorf("pgpmail: parse signer key: %w", err)
		}
		if err := verifyKeys.AddKey(key); err != nil {
			return nil, fmt.Errorf("pgpmail: add signer key: %w", err)
		}
	}
	verifyHandle, err := crypto.PGP().Verify().VerificationKeys(verifyKeys).New()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build verify handle: %w", err)
	}
	result, err := verifyHandle.VerifyDetached(data, []byte(armoredSignature), crypto.Auto)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: verify detached: %w", err)
	}
	out := &VerifyResult{Verified: result.SignatureError() == nil}
	if key := result.SignedByKey(); key != nil {
		out.SignerFingerprint = key.GetFingerprint()
	}
	return out, nil
}

// ParseContent decodes a decrypted PGP/MIME content part (a Content-Type
// header line, a blank line, and a body — either a single text part or a
// multipart/mixed structure exactly like mailmsg.Message.Build() produces)
// into a display body and any attachments. Third-party PGP/MIME senders
// whose inner structure differs (e.g. nested multipart/alternative) degrade
// gracefully: recognized text/attachment parts are extracted, anything else
// is skipped rather than erroring, so the message still renders.
func ParseContent(content []byte) (body string, attachments []mailmsg.Attachment, err error) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(content)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && header == nil {
		return "", nil, fmt.Errorf("pgpmail: read content headers: %w", err)
	}
	rest, err := io.ReadAll(reader.R)
	if err != nil {
		return "", nil, fmt.Errorf("pgpmail: read content body: %w", err)
	}

	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return string(rest), nil, nil
	}

	mr := multipart.NewReader(bytes.NewReader(rest), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return body, attachments, fmt.Errorf("pgpmail: read multipart part: %w", err)
		}
		partBody, err := io.ReadAll(part)
		if err != nil {
			return body, attachments, fmt.Errorf("pgpmail: read part body: %w", err)
		}
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "base64") {
			if decoded, decErr := base64.StdEncoding.DecodeString(string(partBody)); decErr == nil {
				partBody = decoded
			}
		}

		partType := part.Header.Get("Content-Type")
		if body == "" && (strings.HasPrefix(partType, "text/plain") || strings.HasPrefix(partType, "text/html") || partType == "") {
			body = string(partBody)
			continue
		}
		name := part.FileName()
		if name == "" {
			name = "attachment"
		}
		attachments = append(attachments, mailmsg.Attachment{
			Name:     name,
			MimeType: partType,
			Content:  partBody,
		})
	}
	return body, attachments, nil
}
