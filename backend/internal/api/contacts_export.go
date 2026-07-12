package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	vcard "github.com/emersion/go-vcard"
)

// handleContactsExport exports all contacts in the caller's own address book
// as either vCard (.vcf) or CSV format.
func (s *Server) handleContactsExport(w http.ResponseWriter, r *http.Request) {
	store, err := s.contactsFor(r)
	if err != nil {
		http.Error(w, "failed to open contacts store", http.StatusInternalServerError)
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "vcard"
	}

	list := store.List()

	switch format {
	case "vcard":
		w.Header().Set("Content-Type", "text/vcard")
		w.Header().Set("Content-Disposition", `attachment; filename="contacts.vcf"`)
		encoder := vcard.NewEncoder(w)
		for _, contact := range list {
			if contact.Deleted {
				continue
			}
			card := contactToVCard(contact)
			if err := encoder.Encode(card); err != nil {
				http.Error(w, "failed to encode vcard", http.StatusInternalServerError)
				return
			}
		}
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="contacts.csv"`)
		writer := csv.NewWriter(w)
		defer writer.Flush()

		writer.Write([]string{"Name", "Organization", "Title", "Email(s)", "Phone(s)", "Notes", "Birthday"})

		for _, c := range list {
			if c.Deleted {
				continue
			}
			emails := ""
			if len(c.Emails) > 0 {
				emailVals := make([]string, len(c.Emails))
				for i, e := range c.Emails {
					emailVals[i] = e.Value
				}
				emails = strings.Join(emailVals, ";")
			}

			phones := ""
			if len(c.Phones) > 0 {
				phoneVals := make([]string, len(c.Phones))
				for i, p := range c.Phones {
					phoneVals[i] = p.Value
				}
				phones = strings.Join(phoneVals, ";")
			}

			if err := writer.Write([]string{
				c.FormattedName,
				c.Org,
				c.Title,
				emails,
				phones,
				c.Notes,
				c.Birthday,
			}); err != nil {
				http.Error(w, "failed to write csv", http.StatusInternalServerError)
				return
			}
		}
	default:
		http.Error(w, "unsupported format", http.StatusBadRequest)
	}
}

// handleContactsImport imports contacts in vCard format into the caller's own
// address book from a multipart file upload.
func (s *Server) handleContactsImport(w http.ResponseWriter, r *http.Request) {
	store, err := s.contactsFor(r)
	if err != nil {
		http.Error(w, "failed to open contacts store", http.StatusInternalServerError)
		return
	}

	// Limit to 10 MB for import file
	limitedBody := io.LimitReader(r.Body, 10<<20)

	decoder := vcard.NewDecoder(limitedBody)

	type importResult struct {
		Imported int   `json:"imported"`
		Skipped  int   `json:"skipped"`
		Errors   []string `json:"errors"`
	}

	result := importResult{Errors: []string{}}
	maxCards := 5000
	cardCount := 0

	for {
		if cardCount >= maxCards {
			result.Errors = append(result.Errors, fmt.Sprintf("stopped processing after %d contacts (limit reached)", maxCards))
			break
		}

		card, err := decoder.Decode()
		if err != nil {
			if err == io.EOF {
				break
			}
			result.Errors = append(result.Errors, fmt.Sprintf("decode error: %v", err))
			continue
		}
		cardCount++

		contact := contactFromVCard("", card)
		if contact.FormattedName == "" {
			result.Skipped++
			continue
		}

		_, err = store.Upsert(contact)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("import error for %s: %v", contact.FormattedName, err))
			result.Skipped++
			continue
		}
		result.Imported++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
