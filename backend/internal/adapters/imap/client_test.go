package imap

import (
	"reflect"
	"strings"
	"testing"

	goimap "github.com/BrianLeishman/go-imap"

	"kypost-server/backend/internal/mailmsg"
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

// TestEmailContentSize exercises the size-accounting GetMessageBodies and
// fetchAttachments both gate on before ever handing content back to a
// caller. GetMessageBodies/fetchAttachments/GetAttachment themselves can't
// be driven directly in this package's tests without a live (or fake)
// *goimap.Dialer connection — none exists in this test suite today, and
// standing one up is out of scope for this change — so this pins down the
// exact arithmetic those call sites depend on: the total of HTML, text, and
// every attachment's content, which is what a real oversized message would
// trip.
func TestEmailContentSize(t *testing.T) {
	t.Run("sums HTML, text, and every attachment", func(t *testing.T) {
		e := &goimap.Email{
			HTML: strings.Repeat("h", 10),
			Text: strings.Repeat("t", 5),
			Attachments: []goimap.Attachment{
				{Content: make([]byte, 3)},
				{Content: make([]byte, 7)},
			},
		}
		got := emailContentSize(e)
		want := int64(10 + 5 + 3 + 7)
		if got != want {
			t.Fatalf("got %d, want %d", got, want)
		}
	})

	t.Run("empty email sizes to zero", func(t *testing.T) {
		if got := emailContentSize(&goimap.Email{}); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})

	t.Run("many small attachments add up like one big one", func(t *testing.T) {
		withLoweredMaxInboundMessageBytes(t, 20)
		e := &goimap.Email{}
		for i := 0; i < 5; i++ {
			e.Attachments = append(e.Attachments, goimap.Attachment{Content: make([]byte, 5)})
		}
		// 5 attachments * 5 bytes = 25 bytes total, over the 20-byte cap,
		// even though no single attachment is anywhere near it.
		if size := emailContentSize(e); size <= mailmsg.MaxInboundMessageBytes {
			t.Fatalf("expected aggregate size %d to exceed cap %d", size, mailmsg.MaxInboundMessageBytes)
		}
	})

	t.Run("exactly at the cap does not overflow it", func(t *testing.T) {
		withLoweredMaxInboundMessageBytes(t, 16)
		e := &goimap.Email{HTML: strings.Repeat("a", 16)}
		if size := emailContentSize(e); size > mailmsg.MaxInboundMessageBytes {
			t.Fatalf("expected size %d not to exceed cap %d", size, mailmsg.MaxInboundMessageBytes)
		}
	})
}

// TestPartitionUIDsBySize exercises the pure split ListUnreadInbox uses to
// decide which UIDs are safe to pass to go-imap's GetEmails (which fully
// buffers each message's body/attachments into memory) versus which were
// already identified as oversized by the server-side "UNSEEN LARGER <cap>"
// SEARCH and so must never reach GetEmails at all.
//
// This is the seam that proves the expensive fetch is genuinely skipped for
// an oversized UID — not just checked-and-discarded afterwards: toFetch is
// the exact, and only, slice ListUnreadInbox passes to GetEmails, so any UID
// this function places in tooLarge instead structurally cannot appear in
// that call. ListUnreadInbox itself can't be driven end-to-end in this
// package's tests without a live/fake *goimap.Dialer (see TestEmailContentSize
// above), so partitionUIDsBySize is deliberately factored out as ordinary,
// connection-free logic to make that guarantee directly testable.
func TestPartitionUIDsBySize(t *testing.T) {
	t.Run("oversized UIDs are excluded from toFetch and routed to tooLarge", func(t *testing.T) {
		filtered := []int{1, 2, 3, 4, 5}
		oversized := []int{2, 4}

		toFetch, tooLarge := partitionUIDsBySize(filtered, oversized)

		wantFetch := []int{1, 3, 5}
		wantLarge := []int{2, 4}
		if !reflect.DeepEqual(toFetch, wantFetch) {
			t.Fatalf("toFetch = %v, want %v", toFetch, wantFetch)
		}
		if !reflect.DeepEqual(tooLarge, wantLarge) {
			t.Fatalf("tooLarge = %v, want %v", tooLarge, wantLarge)
		}
		// The defining property: no UID present in tooLarge may also be
		// present in toFetch — that would mean an oversized message's body
		// still gets fetched.
		large := make(map[int]bool, len(tooLarge))
		for _, uid := range tooLarge {
			large[uid] = true
		}
		for _, uid := range toFetch {
			if large[uid] {
				t.Fatalf("uid %d present in both toFetch and tooLarge", uid)
			}
		}
	})

	t.Run("no oversized UIDs: everything goes to toFetch", func(t *testing.T) {
		filtered := []int{10, 20, 30}
		toFetch, tooLarge := partitionUIDsBySize(filtered, nil)
		if !reflect.DeepEqual(toFetch, filtered) {
			t.Fatalf("toFetch = %v, want %v", toFetch, filtered)
		}
		if len(tooLarge) != 0 {
			t.Fatalf("expected no oversized UIDs, got %v", tooLarge)
		}
	})

	t.Run("all UIDs oversized: toFetch is empty", func(t *testing.T) {
		filtered := []int{7, 8}
		toFetch, tooLarge := partitionUIDsBySize(filtered, []int{7, 8})
		if len(toFetch) != 0 {
			t.Fatalf("expected empty toFetch, got %v", toFetch)
		}
		if !reflect.DeepEqual(tooLarge, filtered) {
			t.Fatalf("tooLarge = %v, want %v", tooLarge, filtered)
		}
	})

	t.Run("oversized UID outside this batch is ignored, not fabricated", func(t *testing.T) {
		// The LARGER search is scoped by UNSEEN but not by the checkpoint
		// filter, so it can report a UID that raced out of this batch
		// between the two round trips (e.g. no longer unseen). It must not
		// show up in either output slice.
		filtered := []int{1, 2}
		oversized := []int{2, 99}
		toFetch, tooLarge := partitionUIDsBySize(filtered, oversized)
		if !reflect.DeepEqual(toFetch, []int{1}) {
			t.Fatalf("toFetch = %v, want [1]", toFetch)
		}
		if !reflect.DeepEqual(tooLarge, []int{2}) {
			t.Fatalf("tooLarge = %v, want [2]", tooLarge)
		}
	})

	t.Run("empty filtered returns empty slices regardless of oversized input", func(t *testing.T) {
		toFetch, tooLarge := partitionUIDsBySize(nil, []int{1, 2, 3})
		if len(toFetch) != 0 || len(tooLarge) != 0 {
			t.Fatalf("expected both empty, got toFetch=%v tooLarge=%v", toFetch, tooLarge)
		}
	})
}
