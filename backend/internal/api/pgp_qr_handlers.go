package api

import (
	"net/http"
	"strings"
	"time"

	"kypost-server/backend/internal/contacts"
)

// handlePGPQRToken mints a short-TTL token (session auth) that a scanning
// device can exchange for the caller's public key via handlePGPQRKey — used
// for in-person QR-based contact key exchange.
func (s *Server) handlePGPQRToken(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}

	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusBadRequest)
		return
	}
	token, expiresAt, err := s.createPairingToken(ac.UserID, 2*time.Minute)
	if err != nil {
		http.Error(w, "failed to create qr token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"expiresAt": expiresAt.Format(time.RFC3339),
		"url":       s.pickupBaseURL() + "/api/pgp/qr/key?t=" + token,
	})
}

// handlePGPQRKey is public and token-gated (no session): it returns the
// token owner's armored public key + display name, for a scanning device to
// offer as a new/updated contact PGP key.
func (s *Server) handlePGPQRKey(w http.ResponseWriter, r *http.Request) {
	if s.pairingSecret == "" {
		http.Error(w, "pairing is not configured", http.StatusServiceUnavailable)
		return
	}

	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	userID, err := s.parsePairingTokenUserID(token, time.Now())
	if err != nil {
		http.Error(w, "invalid or expired token", http.StatusForbidden)
		return
	}
	u, err := s.users.Get(userID)
	if err != nil || u.PGPFingerprint == "" {
		http.Error(w, "no pgp identity configured", http.StatusNotFound)
		return
	}
	resp := map[string]any{
		"name":        u.Username,
		"fingerprint": u.PGPFingerprint,
		"publicKey":   u.PGPPublicKey,
	}
	if store, err := s.userContactsStore(userID); err == nil {
		if self, ok := store.GetSelf(); ok {
			resp["contactCard"] = contactCardFromContact(self)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// pgpQRContactCard is the shareable subset of contacts.Contact included in
// the QR key-exchange response when the token owner has flagged a contact
// as their own (contacts.Contact.IsSelf). photoRef is deliberately excluded
// — contact photos are served from an authenticated route
// (GET /api/contacts/{id}/photo) that the scanning device, which
// authenticates with nothing but this token, has no session for. Bookkeeping
// and identity fields (uid, rev, isSelf, the contact's own pgpKey sub-field,
// merge markers) are excluded too: none are meaningful to a scanner, and the
// real PGP identity already rides this response's top-level
// fingerprint/publicKey, not whatever the contact's own pgpKey field holds.
type pgpQRContactCard struct {
	FormattedName      string                        `json:"fn,omitempty"`
	GivenName          string                        `json:"givenName,omitempty"`
	FamilyName         string                        `json:"familyName,omitempty"`
	MiddleName         string                        `json:"middleName,omitempty"`
	Prefix             string                        `json:"prefix,omitempty"`
	Suffix             string                        `json:"suffix,omitempty"`
	Nickname           string                        `json:"nickname,omitempty"`
	Org                string                        `json:"org,omitempty"`
	Title              string                        `json:"title,omitempty"`
	Emails             []contacts.ContactValue       `json:"emails,omitempty"`
	Phones             []contacts.ContactValue       `json:"phones,omitempty"`
	Addresses          []contacts.ContactAddress     `json:"addresses,omitempty"`
	Notes              string                        `json:"notes,omitempty"`
	Birthday           string                        `json:"birthday,omitempty"`
	IMs                []contacts.ContactIM          `json:"ims,omitempty"`
	Websites           []contacts.ContactURL         `json:"websites,omitempty"`
	Relations          []contacts.ContactRelation    `json:"relations,omitempty"`
	Events             []contacts.ContactEvent       `json:"events,omitempty"`
	PhoneticGivenName  string                        `json:"phoneticGivenName,omitempty"`
	PhoneticFamilyName string                        `json:"phoneticFamilyName,omitempty"`
	Department         string                        `json:"department,omitempty"`
	CustomFields       []contacts.ContactCustomField `json:"customFields,omitempty"`
	Pronouns           string                        `json:"pronouns,omitempty"`
}

func contactCardFromContact(c contacts.Contact) pgpQRContactCard {
	return pgpQRContactCard{
		FormattedName:      c.FormattedName,
		GivenName:          c.GivenName,
		FamilyName:         c.FamilyName,
		MiddleName:         c.MiddleName,
		Prefix:             c.Prefix,
		Suffix:             c.Suffix,
		Nickname:           c.Nickname,
		Org:                c.Org,
		Title:              c.Title,
		Emails:             c.Emails,
		Phones:             c.Phones,
		Addresses:          c.Addresses,
		Notes:              c.Notes,
		Birthday:           c.Birthday,
		IMs:                c.IMs,
		Websites:           c.Websites,
		Relations:          c.Relations,
		Events:             c.Events,
		PhoneticGivenName:  c.PhoneticGivenName,
		PhoneticFamilyName: c.PhoneticFamilyName,
		Department:         c.Department,
		CustomFields:       c.CustomFields,
		Pronouns:           c.Pronouns,
	}
}
