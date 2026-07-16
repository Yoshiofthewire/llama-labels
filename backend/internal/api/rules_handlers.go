package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"llama-lab/backend/internal/rules"
)

type rulePayload struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Enabled     bool             `json:"enabled"`
	Order       int              `json:"order"`
	Scope       rules.RuleScope  `json:"scope"`
	Match       rules.MatchGroup `json:"match"`
	Actions     []rules.Action   `json:"actions"`
	Rev         int64            `json:"rev"`
	CreatedAt   string           `json:"createdAt"`
	UpdatedAt   string           `json:"updatedAt"`
	GUIEditable bool             `json:"guiEditable"`
}

// ruleIsGUIEditable reports whether r's Match tree is flat enough for the
// GUI builder: depth 1, i.e. no Condition.Group anywhere. Rules produced by
// hand-edited Sieve can nest arbitrarily deep and are reported as
// script-only.
func ruleIsGUIEditable(r rules.Rule) bool {
	for _, c := range r.Match.Conditions {
		if c.Group != nil {
			return false
		}
	}
	return true
}

func ruleToPayload(r rules.Rule) rulePayload {
	return rulePayload{
		ID:          r.ID,
		Name:        r.Name,
		Enabled:     r.Enabled,
		Order:       r.Order,
		Scope:       r.Scope,
		Match:       r.Match,
		Actions:     r.Actions,
		Rev:         r.Rev,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
		GUIEditable: ruleIsGUIEditable(r),
	}
}

func ruleFromPayload(p rulePayload) rules.Rule {
	return rules.Rule{
		ID:      p.ID,
		Name:    p.Name,
		Enabled: p.Enabled,
		Order:   p.Order,
		Scope:   p.Scope,
		Match:   p.Match,
		Actions: p.Actions,
	}
}

// handleRules serves the caller's rule list and creates new rules.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	store, err := s.rulesFor(r)
	if err != nil {
		http.Error(w, "failed to open rules store", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := store.List()
		out := make([]rulePayload, 0, len(list))
		for _, rl := range list {
			out = append(out, ruleToPayload(rl))
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": out})
	case http.MethodPost:
		var payload rulePayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(payload.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		payload.ID = ""
		payload.Name = name
		created, err := store.Upsert(ruleFromPayload(payload))
		if err != nil {
			http.Error(w, "failed to create rule", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, ruleToPayload(created))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRuleByID updates or deletes a single rule.
func (s *Server) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	store, err := s.rulesFor(r)
	if err != nil {
		http.Error(w, "failed to open rules store", http.StatusInternalServerError)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		existing, ok := store.Get(id)
		if !ok {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		var payload rulePayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(payload.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		payload.ID = existing.ID
		payload.Name = name
		// Order and Scope aren't part of the editable payload for this
		// endpoint (Order is managed by /reorder; Scope isn't settable via
		// the GUI editor) — base on existing so a partial update (e.g. a
		// client that only sends name/match/actions) doesn't silently zero
		// them out via Go's int/struct zero values.
		payload.Order = existing.Order
		payload.Scope = existing.Scope
		updated, err := store.Upsert(ruleFromPayload(payload))
		if err != nil {
			http.Error(w, "failed to update rule", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, ruleToPayload(updated))
	case http.MethodDelete:
		removed, err := store.Delete(id)
		if err != nil {
			http.Error(w, "failed to delete rule", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRulesReorder persists a new Order for each named rule ID, in the
// order given.
func (s *Server) handleRulesReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, err := s.rulesFor(r)
	if err != nil {
		http.Error(w, "failed to open rules store", http.StatusInternalServerError)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "ids is required", http.StatusBadRequest)
		return
	}
	if err := store.Reorder(req.IDs); err != nil {
		http.Error(w, "failed to reorder rules", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRuleSieve serves (GET) and accepts (PUT) the Sieve-subset text view
// of one rule. GET compiles Match/Actions to script text; PUT parses script
// text back into Match/Actions, leaving every other field on the rule
// untouched. A parse error is reported as 400 with the parser's message (it
// already includes a line number) so the frontend can show it inline
// without discarding the user's edit.
func (s *Server) handleRuleSieve(w http.ResponseWriter, r *http.Request) {
	store, err := s.rulesFor(r)
	if err != nil {
		http.Error(w, "failed to open rules store", http.StatusInternalServerError)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	existing, ok := store.Get(id)
	if !ok {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		script, err := rules.CompileRule(existing)
		if err != nil {
			http.Error(w, "failed to compile rule", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"script": script})
	case http.MethodPut:
		var req struct {
			Script string `json:"script"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		parsed, err := rules.ParseRuleText(req.Script, existing)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		updated, err := store.Upsert(parsed)
		if err != nil {
			http.Error(w, "failed to save rule", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, ruleToPayload(updated))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
