package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	imapadapter "llama-lab/backend/internal/adapters/imap"
	"llama-lab/backend/internal/rules"
)

func TestRulesCreateListUpdateDelete(t *testing.T) {
	srv := newTestServer(t)

	createBody, _ := json.Marshal(map[string]any{
		"name":    "Archive newsletters",
		"enabled": true,
		"match": map[string]any{
			"op": "allof",
			"conditions": []map[string]any{
				{"field": "from", "comparator": "contains", "value": "newsletter"},
			},
		},
		"actions": []map[string]any{{"type": "archive"}},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(createBody))
	authRequest(srv, createReq)
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", createRec.Code, createRec.Body.String())
	}
	var created rulePayload
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected assigned ID")
	}
	if !created.GUIEditable {
		t.Fatal("expected flat rule to be GUI editable")
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	authRequest(srv, listReq)
	listRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Rules []rulePayload `json:"rules"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(listResp.Rules))
	}

	updateBody, _ := json.Marshal(map[string]any{
		"name":    "Archive all newsletters",
		"enabled": false,
		"match":   listResp.Rules[0].Match,
		"actions": listResp.Rules[0].Actions,
	})
	updateReq := httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID, bytes.NewReader(updateBody))
	authRequest(srv, updateReq)
	updateRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", updateRec.Code, updateRec.Body.String())
	}
	var updated rulePayload
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Name != "Archive all newsletters" || updated.Enabled {
		t.Fatalf("update did not apply, got %+v", updated)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/rules/"+created.ID, nil)
	authRequest(srv, deleteReq)
	deleteRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestRulesSieveRoundTrip(t *testing.T) {
	srv := newTestServer(t)

	createBody, _ := json.Marshal(map[string]any{
		"name": "from acme",
		"match": map[string]any{
			"op":         "allof",
			"conditions": []map[string]any{{"field": "from", "comparator": "contains", "value": "acme"}},
		},
		"actions": []map[string]any{{"type": "move", "value": "Archive/Acme"}},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(createBody))
	authRequest(srv, createReq)
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	var created rulePayload
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/rules/"+created.ID+"/sieve", nil)
	authRequest(srv, getReq)
	getRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get sieve status = %d, body=%s", getRec.Code, getRec.Body.String())
	}
	var sieveResp struct {
		Script string `json:"script"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &sieveResp); err != nil {
		t.Fatalf("decode sieve response: %v", err)
	}
	if sieveResp.Script == "" {
		t.Fatal("expected non-empty compiled script")
	}

	putBody, _ := json.Marshal(map[string]any{"script": sieveResp.Script})
	putReq := httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID+"/sieve", bytes.NewReader(putBody))
	authRequest(srv, putReq)
	putRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put sieve status = %d, body=%s", putRec.Code, putRec.Body.String())
	}

	badReq := httptest.NewRequest(http.MethodPut, "/api/rules/"+created.ID+"/sieve", bytes.NewReader([]byte(`{"script":"if bogus(x) { keep; }"}`)))
	authRequest(srv, badReq)
	badRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad script, got %d, body=%s", badRec.Code, badRec.Body.String())
	}
}

func TestRulesReorder(t *testing.T) {
	srv := newTestServer(t)
	all, _ := srv.users.List()
	store, err := srv.userRulesStore(all[0].ID)
	if err != nil {
		t.Fatalf("userRulesStore: %v", err)
	}
	a, _ := store.Upsert(rulesTestRule("a"))
	b, _ := store.Upsert(rulesTestRule("b"))

	reorderBody, _ := json.Marshal(map[string]any{"ids": []string{b.ID, a.ID}})
	req := httptest.NewRequest(http.MethodPost, "/api/rules/reorder", bytes.NewReader(reorderBody))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status = %d, body=%s", rec.Code, rec.Body.String())
	}

	list := store.List()
	if len(list) != 2 || list[0].Name != "b" || list[1].Name != "a" {
		t.Fatalf("expected [b, a] after reorder, got %+v", list)
	}
}

// TestRulesUpdatePreservesOrder guards against a regression where PUT
// /api/rules/{id} zeroed out a rule's Order whenever the request payload
// omitted it (Go zero-values a missing "order" field to 0, and Store.Upsert
// only defaults Order on create, not update). With a single rule in the
// store the rule's Order is already 0, so that scenario alone wouldn't catch
// the bug: this test seeds two rules with distinct, non-zero-at-index Order
// values and updates the second one with a payload that has no "order" key
// at all, then asserts both rules' Order/relative ordering survive.
func TestRulesUpdatePreservesOrder(t *testing.T) {
	srv := newTestServer(t)
	all, _ := srv.users.List()
	store, err := srv.userRulesStore(all[0].ID)
	if err != nil {
		t.Fatalf("userRulesStore: %v", err)
	}
	first, err := store.Upsert(rulesTestRule("first"))
	if err != nil {
		t.Fatalf("seed first rule: %v", err)
	}
	second, err := store.Upsert(rulesTestRule("second"))
	if err != nil {
		t.Fatalf("seed second rule: %v", err)
	}
	if first.Order != 0 || second.Order != 1 {
		t.Fatalf("expected seeded orders [0, 1], got first=%d second=%d", first.Order, second.Order)
	}

	// Deliberately omit "order" (and "scope"), as a partial edit form would.
	updateBody, _ := json.Marshal(map[string]any{
		"name":    "second updated",
		"enabled": true,
		"match":   ruleToPayload(second).Match,
		"actions": ruleToPayload(second).Actions,
	})
	updateReq := httptest.NewRequest(http.MethodPut, "/api/rules/"+second.ID, bytes.NewReader(updateBody))
	authRequest(srv, updateReq)
	updateRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", updateRec.Code, updateRec.Body.String())
	}
	var updated rulePayload
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Name != "second updated" {
		t.Fatalf("update did not apply name change, got %+v", updated)
	}
	if updated.Order != 1 {
		t.Fatalf("expected updated rule's Order to remain 1, got %d", updated.Order)
	}

	list := store.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(list))
	}
	if list[0].Name != "first" || list[0].Order != 0 {
		t.Fatalf("expected first rule unaffected at Order 0, got %+v", list[0])
	}
	if list[1].Name != "second updated" || list[1].Order != 1 {
		t.Fatalf("expected second rule updated in place at Order 1, got %+v", list[1])
	}
}

func TestRulesRun_AppliesMatchingRuleAndSkipsNonMatching(t *testing.T) {
	srv := newTestServer(t)
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	all, _ := srv.users.List()
	userID := all[0].ID

	store, err := srv.userRulesStore(userID)
	if err != nil {
		t.Fatalf("userRulesStore: %v", err)
	}
	if _, err := store.Upsert(rulesTestRule("archive acme")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// mailFor/userMailClient only reuses a cached client when the cached
	// updatedAt matches what's on disk — write a real (if inert) IMAP
	// config payload stamped with the same updatedAt used below so the
	// handler's mailFor resolves to the injected fake instead of building a
	// real imapadapter.APIClient.
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw",
		Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatalf("writeIMAPConfigPayload: %v", err)
	}
	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{MessageID: "1", UID: 1, Sender: "billing@acme.com", Subject: "Invoice"},
			{MessageID: "2", UID: 2, Sender: "someone@example.com", Subject: "Hello"},
		},
	}
	srv.userMu.Lock()
	srv.userMail[userID] = &serverMailEntry{client: fake, updatedAt: "test"}
	srv.userMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/rules/run", bytes.NewReader([]byte(`{"mailbox":"INBOX"}`)))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result rulesRunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Scanned != 2 || result.Matched != 1 || result.Applied != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want scanned=2 matched=1 applied=1 failed=0", result)
	}
}

// rulesTestRule builds a minimal enabled rule matching From contains "acme"
// with an archive action, for tests that only need a fixture rule to exist.
// Task 7 adds a second test function to this file that also uses this
// helper — keep it exported at file scope, not local to one test.
func rulesTestRule(name string) rules.Rule {
	return rules.Rule{
		Name:    name,
		Enabled: true,
		Match: rules.MatchGroup{
			Op:         "allof",
			Conditions: []rules.Condition{{Field: "from", Comparator: "contains", Value: "acme"}},
		},
		Actions: []rules.Action{{Type: "archive"}},
	}
}
