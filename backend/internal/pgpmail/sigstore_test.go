package pgpmail

import "testing"

func TestSigStorePutGet(t *testing.T) {
	store := NewSigStore(t.TempDir())

	if _, ok := store.Get("msg-1"); ok {
		t.Fatal("expected no record before Put")
	}

	record := SignatureRecord{
		MessageID:         "msg-1",
		SignerFingerprint: "ABCDEF",
		Verified:          true,
		VerifiedAt:        "2026-07-14T00:00:00Z",
	}
	if err := store.Put(record); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := store.Get("msg-1")
	if !ok {
		t.Fatal("expected record after Put")
	}
	if got != record {
		t.Fatalf("record mismatch: got %+v want %+v", got, record)
	}
}

func TestSigStorePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	first := NewSigStore(dir)
	if err := first.Put(SignatureRecord{MessageID: "msg-2", SignerFingerprint: "111", Verified: false, VerifiedAt: "2026-07-14T00:00:00Z"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	second := NewSigStore(dir)
	got, ok := second.Get("msg-2")
	if !ok || got.SignerFingerprint != "111" {
		t.Fatalf("expected record to persist across instances, got %+v (ok=%v)", got, ok)
	}
}
