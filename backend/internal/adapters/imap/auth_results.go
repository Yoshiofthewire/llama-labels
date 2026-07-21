package imap

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	goimap "github.com/BrianLeishman/go-imap"
)

// FetchHeaderFields issues a raw UID FETCH for BODY.PEEK[HEADER.FIELDS (...)]
// against uids in the currently selected mailbox — a header-only fetch, not
// a full body fetch, since callers of this method only need to inspect
// specific headers (e.g. Authentication-Results) cheaply. Returns, per UID,
// every unfolded header line whose field name matches one of fields
// (case-insensitive), each still carrying its "Field-Name: value" prefix so
// a caller requesting multiple fields can tell them apart. A UID with none
// of the requested fields present is simply absent from the result — not an
// error.
func (c *APIClient) FetchHeaderFields(ctx context.Context, uids []int, fields ...string) (map[int][]string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return map[int][]string{}, nil
	}

	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}

	var uidsStr strings.Builder
	for i, u := range uids {
		if i != 0 {
			uidsStr.WriteByte(',')
		}
		uidsStr.WriteString(strconv.Itoa(u))
	}

	fieldList := strings.Join(fields, " ")
	cmd := "UID FETCH " + uidsStr.String() + " BODY.PEEK[HEADER.FIELDS (" + fieldList + ")]"

	raw, err := d.Exec(cmd, true, goimap.RetryCount, nil)
	if err != nil {
		return nil, fmt.Errorf("imap fetch header fields: %w", err)
	}

	records, err := d.ParseFetchResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("imap fetch header fields: %w", err)
	}

	out, err := parseHeaderFieldsRecords(records, fields)
	if err != nil {
		return nil, fmt.Errorf("imap fetch header fields: %w", err)
	}
	return out, nil
}

// unwrapTokens flattens single-child TContainer wrappers that some servers
// add. Replicated locally because the vendored go-imap version keeps its own
// copy unexported (message.go:397-403).
func unwrapTokens(tks []*goimap.Token) []*goimap.Token {
	for len(tks) == 1 && tks[0].Type == goimap.TContainer {
		tks = tks[0].Tokens
	}
	return tks
}

// parseHeaderFieldsRecords extracts, per UID, every unfolded header line whose
// field name matches one of fields (case-insensitive) from parsed FETCH
// records — the pure logic behind FetchHeaderFields, split out so it can be
// tested directly against synthetic records without a live IMAP connection.
//
// NOTE ON THE TOKEN SHAPE (verified empirically against go-imap v0.1.28, not
// merely assumed): a HEADER.FIELDS FETCH record tokenizes to
//
//	UID <TNumber> BODY[HEADER.FIELDS <TContainer(field names)> ] <value>
//
// i.e. the requested-field list becomes a nested TContainer and the trailing
// "]" of the item name becomes its own TLiteral token, so the header value is
// NOT the token immediately after the "BODY[HEADER.FIELDS" marker. The value
// itself arrives as a TAtom (the {NNN} literal body) or TNil (for NIL), never
// as the "]" TLiteral. We therefore locate the marker and then scan forward
// for the first TAtom / TQuoted / TNil token — which cleanly skips both the
// TContainer field list and the "]" TLiteral separator. UID and value are
// collected independently across the whole record because some servers emit
// the UID token after the body item.
func parseHeaderFieldsRecords(records [][]*goimap.Token, fields []string) (map[int][]string, error) {
	result := make(map[int][]string)

	for _, tks := range records {
		tks = unwrapTokens(tks)

		var (
			uid        int
			uidFound   bool
			value      string
			valueFound bool
		)

		for i := 0; i < len(tks); i++ {
			t := tks[i]

			// UID token: the following token holds its numeric value.
			if strings.EqualFold(t.Str, "UID") {
				if i+1 >= len(tks) {
					return nil, fmt.Errorf("parse header fields: UID token has no following value")
				}
				if tks[i+1].Type != goimap.TNumber {
					return nil, fmt.Errorf("parse header fields: expected TNumber after UID, got %s", goimap.GetTokenName(tks[i+1].Type))
				}
				uid = tks[i+1].Num
				uidFound = true
				i++
				continue
			}

			// BODY[HEADER.FIELDS marker: the value is the first TAtom/TQuoted/
			// TNil token that follows (skipping the field-list TContainer and
			// the "]" TLiteral separator).
			if strings.HasPrefix(strings.ToUpper(t.Str), "BODY[HEADER.FIELDS") {
				var got bool
				for j := i + 1; j < len(tks); j++ {
					switch tks[j].Type {
					case goimap.TNil:
						value = ""
						got = true
					case goimap.TAtom, goimap.TQuoted:
						value = tks[j].Str
						got = true
					}
					if got {
						break
					}
				}
				if !got {
					return nil, fmt.Errorf("parse header fields: BODY[HEADER.FIELDS token has no value")
				}
				valueFound = true
			}
		}

		if !uidFound {
			return nil, fmt.Errorf("parse header fields: record has no UID token")
		}

		// A UID whose header-fields value is empty/NIL (no requested field
		// present) is simply absent from the result — that is the normal,
		// common case, not an error.
		if !valueFound || value == "" {
			continue
		}

		lines := unfoldHeaderLines(value)
		if len(lines) > 0 {
			result[uid] = append(result[uid], lines...)
		}
	}

	return result, nil
}

// unfoldHeaderLines splits a raw HEADER.FIELDS block into individual unfolded
// logical header lines (RFC 5322 §2.2.3). A physical line that starts with a
// space or tab is a continuation of the previous logical header; the line
// break plus the continuation line's leading run of spaces/tabs collapses to a
// single space. Lines that do not start with whitespace begin a new logical
// header. Blank lines (including the CRLF-CRLF block terminator) are discarded
// rather than emitted as empty headers. Distinct occurrences of the same
// header name become separate strings, in the order they appear.
func unfoldHeaderLines(block string) []string {
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.ReplaceAll(block, "\r", "\n")

	var lines []string
	var cur strings.Builder
	have := false

	flush := func() {
		if have {
			lines = append(lines, cur.String())
			cur.Reset()
			have = false
		}
	}

	for _, phys := range strings.Split(block, "\n") {
		switch {
		case phys == "":
			// Blank line: block terminator or separator — discard.
			flush()
		case phys[0] == ' ' || phys[0] == '\t':
			// Continuation of the current logical header.
			cont := strings.TrimLeft(phys, " \t")
			if have {
				cur.WriteByte(' ')
			}
			cur.WriteString(cont)
			have = true
		default:
			// Start of a new logical header.
			flush()
			cur.WriteString(phys)
			have = true
		}
	}
	flush()

	return lines
}

var (
	// dkimPassRe captures the header.d= value of a passing DKIM result. The
	// [^;]*? between "dkim=pass" and "header.d=" keeps the property bound to
	// the same ;-delimited result clause, so a "dkim=fail" result for a
	// different domain in the same header can't lend its header.d= to a
	// passing result for another domain.
	dkimPassRe = regexp.MustCompile(`(?i)dkim\s*=\s*pass\b[^;]*?\bheader\.d\s*=\s*([^\s;)]+)`)

	// spfPassRe captures the smtp.mailfrom= / smtp.helo= identity of a passing
	// SPF result, likewise scoped to the same clause as the pass verdict.
	spfPassRe = regexp.MustCompile(`(?i)spf\s*=\s*pass\b[^;]*?\bsmtp\.(mailfrom|helo)\s*=\s*([^\s;)]+)`)
)

// AuthenticationResultsPassForDomain reports whether the topmost
// Authentication-Results header line — headerLines[0], assumed to have been
// added by the final, trusted delivering MTA (the account's own mailbox
// provider) — shows a DKIM or SPF "pass" verdict scoped to domain.
//
// Only index 0 is ever examined. This is deliberate, not an oversight: a
// message can accumulate more than one Authentication-Results header as it
// transits multiple hops, and an untrusted upstream hop could inject its own
// forged header claiming a pass verdict for any domain it likes. Only the
// header believed to have been stamped last — by the receiving side closest
// to final delivery, which is the only one whose verdict this account's
// provider is actually vouching for — is ever consulted. headerLines is
// expected in the wire order FetchHeaderFields/parseHeaderFieldsRecords
// returns it (topmost-first).
//
// Fails closed: returns false if headerLines is empty, the topmost line
// doesn't parse, or neither a passing DKIM header.d= nor a passing SPF
// smtp.mailfrom=/smtp.helo= matches domain exactly (case-insensitive; no
// subdomain matching in this version — a stricter-than-necessary default
// consistent with this feature's fail-closed design throughout).
func AuthenticationResultsPassForDomain(headerLines []string, domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return false
	}
	if len(headerLines) == 0 {
		return false
	}

	// Only the topmost header line is ever trusted. Do not loop over the rest.
	line := headerLines[0]

	const prefix = "authentication-results:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		// Something upstream filtered the wrong header — do not guess.
		return false
	}
	line = line[len(prefix):]

	// Passing DKIM scoped to domain.
	for _, m := range dkimPassRe.FindAllStringSubmatch(line, -1) {
		if strings.ToLower(m[1]) == domain {
			return true
		}
	}

	// Passing SPF scoped to domain.
	for _, m := range spfPassRe.FindAllStringSubmatch(line, -1) {
		prop := strings.ToLower(m[1])
		val := strings.ToLower(m[2])
		if prop == "mailfrom" {
			if at := strings.LastIndexByte(val, '@'); at >= 0 {
				val = val[at+1:]
			}
		}
		if val == domain {
			return true
		}
	}

	return false
}
