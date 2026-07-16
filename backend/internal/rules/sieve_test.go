package rules

import (
	"fmt"
	"strings"
	"testing"
)

// wantCondition describes the expected shape of one parsed Condition,
// including (via group) the nested conditions of a Condition.Group so
// success-case subtests can assert group contents, not just the top level.
type wantCondition struct {
	negate     bool
	field      string
	comparator string
	value      string
	group      []wantCondition // non-nil => Condition.Group expected, checked recursively
}

// assertConditions compares got against want field-by-field (Negate, Field,
// Comparator, Value), recursing into Condition.Group when want.group is set.
// A bug like a comparator tag being silently defaulted instead of read from
// the actual Sieve source would previously slip past TestParseRuleText,
// which only checked len(Match.Conditions); this catches it.
func assertConditions(t *testing.T, path string, got []Condition, want []wantCondition) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len = %d, want %d (%+v)", path, len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		item := fmt.Sprintf("%s[%d]", path, i)
		if w.group != nil {
			if g.Group == nil {
				t.Fatalf("%s: Group = nil, want a nested group %+v", item, w.group)
			}
			assertConditions(t, item+".Group", g.Group.Conditions, w.group)
			continue
		}
		if g.Group != nil {
			t.Fatalf("%s: Group = %+v, want a leaf condition", item, g.Group)
		}
		if g.Negate != w.negate {
			t.Errorf("%s: Negate = %v, want %v", item, g.Negate, w.negate)
		}
		if g.Field != w.field {
			t.Errorf("%s: Field = %q, want %q", item, g.Field, w.field)
		}
		if g.Comparator != w.comparator {
			t.Errorf("%s: Comparator = %q, want %q", item, g.Comparator, w.comparator)
		}
		if g.Value != w.value {
			t.Errorf("%s: Value = %q, want %q", item, g.Value, w.value)
		}
	}
}

func TestParseRuleText(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		wantOp     string
		wantConds  []wantCondition
		wantAction []Action
	}{
		{
			name:   "simple header contains with fileinto",
			script: "require [\"fileinto\"];\n\nif allof(header :contains [\"from\"] \"acme\") {\n    fileinto \"Archive/Acme\";\n}\n",
			wantOp: "allof",
			wantConds: []wantCondition{
				{field: "from", comparator: "contains", value: "acme"},
			},
			wantAction: []Action{{Type: "move", Value: "Archive/Acme"}},
		},
		{
			name:   "anyof with not and hasflag",
			script: "require [\"llamalabs\", \"imap4flags\"];\nif anyof(not header :is [\"subject\"] \"spam\", hasflag :is \"VIP\") {\n    archive;\n    stop;\n}\n",
			wantOp: "anyof",
			wantConds: []wantCondition{
				{negate: true, field: "subject", comparator: "is", value: "spam"},
				{field: "keyword", comparator: "is", value: "VIP"},
			},
			wantAction: []Action{{Type: "archive"}, {Type: "stop"}},
		},
		{
			name:   "keep produces no actions",
			script: "if allof(body :contains \"unsubscribe\") {\n    keep;\n}\n",
			wantOp: "allof",
			wantConds: []wantCondition{
				{field: "body", comparator: "contains", value: "unsubscribe"},
			},
			wantAction: nil,
		},
		{
			name:   "comments are stripped",
			script: "# a leading comment\nif allof(header :contains [\"from\"] \"acme\") { # inline comment\n    /* block\n       comment */\n    fileinto \"X\";\n}\n",
			wantOp: "allof",
			wantConds: []wantCondition{
				{field: "from", comparator: "contains", value: "acme"},
			},
			wantAction: []Action{{Type: "move", Value: "X"}},
		},
		{
			// A header/address test with more than one field name becomes a
			// nested anyof Condition.Group (see fieldsToCondition); this
			// exercises assertConditions' recursion into .group, and also
			// covers the new valid-field-name path (both names are in
			// validHeaderAddressFields) alongside Issue 1's rejection path.
			name:   "header test with multiple fields becomes nested anyof group",
			script: "if allof(header :is [\"from\", \"to\"] \"acme\") {\n    keep;\n}\n",
			wantOp: "allof",
			wantConds: []wantCondition{
				{group: []wantCondition{
					{field: "from", comparator: "is", value: "acme"},
					{field: "to", comparator: "is", value: "acme"},
				}},
			},
			wantAction: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRuleText(tc.script, Rule{ID: "rule-1", Name: "kept"})
			if err != nil {
				t.Fatalf("ParseRuleText: %v", err)
			}
			if got.ID != "rule-1" || got.Name != "kept" {
				t.Fatalf("expected non-Match/Actions fields to pass through, got %+v", got)
			}
			if got.Match.Op != tc.wantOp {
				t.Fatalf("Match.Op = %q, want %q", got.Match.Op, tc.wantOp)
			}
			assertConditions(t, "Match.Conditions", got.Match.Conditions, tc.wantConds)
			if len(got.Actions) != len(tc.wantAction) {
				t.Fatalf("Actions = %+v, want %+v", got.Actions, tc.wantAction)
			}
			for i, want := range tc.wantAction {
				if got.Actions[i] != want {
					t.Errorf("Actions[%d] = %+v, want %+v", i, got.Actions[i], want)
				}
			}
		})
	}
}

func TestParseRuleText_Errors(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		wantSubstr string
	}{
		{
			name:       "unknown test",
			script:     "if bogustest([\"from\"] \"x\") {\n  keep;\n}\n",
			wantSubstr: "unknown test",
		},
		{
			name:       "unknown action",
			script:     "if allof(header :contains [\"from\"] \"x\") {\n  bogusaction;\n}\n",
			wantSubstr: "unknown action",
		},
		{
			name:       "unbalanced braces",
			script:     "if allof(header :contains [\"from\"] \"x\") {\n  keep;\n",
			wantSubstr: "unbalanced braces",
		},
		{
			name:       "unsupported require capability",
			script:     "require [\"nonexistent\"];\nif allof(header :contains [\"from\"] \"x\") {\n  keep;\n}\n",
			wantSubstr: "unsupported require capability",
		},
		{
			// Regression test for Issue 1: "keyword" and "body" are only
			// valid as their own dedicated Sieve test types (hasflag / body
			// test), not as a field name inside a header/address field list.
			// Before this fix, this parsed successfully to
			// Condition{Field:"keyword", ...}, which CompileRule then
			// silently re-emits as a hasflag test — a different Sieve test
			// with different semantics, with no error anywhere.
			name:       "header test with keyword field name is rejected",
			script:     "if allof(header :is [\"keyword\"] \"urgent\") {\n  keep;\n}\n",
			wantSubstr: "unsupported header/address field \"keyword\"",
		},
		{
			// Regression test for the sibling of Issue 1: the exists test
			// path had the identical hazard as header/address. Before this
			// fix, exists ["body"] parsed successfully to
			// Condition{Field:"body", Comparator:"exists", Value:""}, which
			// CompileRule then silently re-emits as `body :is ""` — an
			// always-near-true body-contains-empty-string test, not an
			// existence check — a completely different Sieve test with no
			// error anywhere.
			name:       "exists test with body field name is rejected",
			script:     "if exists [\"body\"] {\n  keep;\n}\n",
			wantSubstr: "unsupported exists field \"body\"",
		},
		{
			name:       "exists test with keyword field name is rejected",
			script:     "if exists [\"keyword\"] {\n  keep;\n}\n",
			wantSubstr: "unsupported exists field \"keyword\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRuleText(tc.script, Rule{})
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestRoundTrip verifies CompileRule(mustParse(CompileRule(r))) is
// semantically equivalent to CompileRule(r) for every GUI-producible rule
// shape (flat allof/anyof of leaf conditions, one condition per field).
func TestRoundTrip(t *testing.T) {
	fixtures := []Rule{
		{
			Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "from", Comparator: "contains", Value: "acme"}}},
			Actions: []Action{{Type: "move", Value: "Archive/Acme"}},
		},
		{
			Match: MatchGroup{Op: "anyof", Conditions: []Condition{
				{Field: "subject", Comparator: "is", Value: "spam", Negate: true},
				{Field: "keyword", Comparator: "contains", Value: "vip"},
				{Field: "body", Comparator: "matches", Value: "*unsubscribe*"},
			}},
			Actions: []Action{{Type: "keyword", Value: "Flagged"}, {Type: "unkeyword", Value: "Unread"}, {Type: "stop"}},
		},
		{
			Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "cc", Comparator: "exists"}}},
			Actions: []Action{{Type: "read"}, {Type: "archive"}, {Type: "spam"}, {Type: "delete"}},
		},
		{
			Match: MatchGroup{Op: "allof", Conditions: []Condition{{Field: "to", Comparator: "regex", Value: "^a.*z$"}}},
		},
	}

	for i, rule := range fixtures {
		first, err := CompileRule(rule)
		if err != nil {
			t.Fatalf("fixture %d: CompileRule: %v", i, err)
		}
		parsed, err := ParseRuleText(first, Rule{})
		if err != nil {
			t.Fatalf("fixture %d: ParseRuleText(%q): %v", i, first, err)
		}
		second, err := CompileRule(parsed)
		if err != nil {
			t.Fatalf("fixture %d: CompileRule(parsed): %v", i, err)
		}
		if first != second {
			t.Fatalf("fixture %d: round trip mismatch:\nfirst:\n%s\nsecond:\n%s", i, first, second)
		}
	}
}

func TestCompileRule(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{
			name: "single condition with move action",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "from", Comparator: "contains", Value: "acme"}}},
				Actions: []Action{{Type: "move", Value: "Archive/Acme"}},
			},
			want: "require [\"fileinto\"];\n\nif allof(header :contains [\"from\"] \"acme\") {\n    fileinto \"Archive/Acme\";\n}\n",
		},
		{
			name: "anyof with negate and stop",
			rule: Rule{
				Match: MatchGroup{Op: "anyof", Conditions: []Condition{
					{Field: "subject", Comparator: "is", Value: "spam", Negate: true},
					{Field: "keyword", Comparator: "is", Value: "VIP"},
				}},
				Actions: []Action{{Type: "archive"}, {Type: "stop"}},
			},
			want: "require [\"imap4flags\", \"llamalabs\"];\n\nif anyof(not header :is [\"subject\"] \"spam\", hasflag :is \"VIP\") {\n    archive;\n    stop;\n}\n",
		},
		{
			name: "no actions renders keep",
			rule: Rule{
				Match: MatchGroup{Op: "allof", Conditions: []Condition{{Field: "body", Comparator: "contains", Value: "unsubscribe"}}},
			},
			want: "require [\"body\"];\n\nif allof(body :contains \"unsubscribe\") {\n    keep;\n}\n",
		},
		{
			name: "regex comparator",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "subject", Comparator: "regex", Value: "^Re:.*invoice$"}}},
				Actions: []Action{{Type: "delete"}},
			},
			want: "require [\"regex\"];\n\nif allof(header :regex [\"subject\"] \"^Re:.*invoice$\") {\n    discard;\n}\n",
		},
		{
			name: "addflag and removeflag",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "to", Comparator: "is", Value: "me@example.com"}}},
				Actions: []Action{{Type: "keyword", Value: "Personal"}, {Type: "unkeyword", Value: "Unread"}},
			},
			want: "require [\"imap4flags\"];\n\nif allof(header :is [\"to\"] \"me@example.com\") {\n    addflag \"Personal\";\n    removeflag \"Unread\";\n}\n",
		},
		{
			name: "exists comparator",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "cc", Comparator: "exists"}}},
				Actions: []Action{{Type: "read"}},
			},
			want: "require [\"llamalabs\"];\n\nif allof(exists [\"cc\"]) {\n    markread;\n}\n",
		},
		{
			name: "markspam",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "bcc", Comparator: "matches", Value: "*@spam.example"}}},
				Actions: []Action{{Type: "spam"}},
			},
			want: "require [\"llamalabs\"];\n\nif allof(header :matches [\"bcc\"] \"*@spam.example\") {\n    markspam;\n}\n",
		},
		{
			// Covers the Condition.Group recursion path: a nested anyof group
			// (with an internal Negate:true leaf) sits alongside a plain leaf
			// condition inside a top-level allof, and the whole nested group
			// condition is itself negated. Also exercises the
			// matchGroupUsesField/matchGroupUsesComparator recursion into
			// Group, since the nested group is what supplies the "keyword"
			// field (imap4flags) and "regex" comparator (regex) capabilities.
			name: "nested group with negate",
			rule: Rule{
				Match: MatchGroup{Op: "allof", Conditions: []Condition{
					{Field: "from", Comparator: "contains", Value: "acme"},
					{
						Negate: true,
						Group: &MatchGroup{Op: "anyof", Conditions: []Condition{
							{Field: "subject", Comparator: "regex", Value: "^Re:", Negate: true},
							{Field: "keyword", Comparator: "is", Value: "VIP"},
						}},
					},
				}},
				Actions: []Action{{Type: "move", Value: "Archive/Important"}},
			},
			want: "require [\"fileinto\", \"regex\", \"imap4flags\"];\n\nif allof(header :contains [\"from\"] \"acme\", not anyof(not header :regex [\"subject\"] \"^Re:\", hasflag :is \"VIP\")) {\n    fileinto \"Archive/Important\";\n}\n",
		},
		{
			// Covers the zero-capability path: subject/contains and stop are
			// both capability-free constructs, so the `if len(caps) > 0`
			// guard around the require line must take its false branch and
			// emit no require statement at all.
			name: "no capabilities used renders no require line",
			rule: Rule{
				Match:   MatchGroup{Op: "allof", Conditions: []Condition{{Field: "subject", Comparator: "contains", Value: "X"}}},
				Actions: []Action{{Type: "stop"}},
			},
			want: "if allof(header :contains [\"subject\"] \"X\") {\n    stop;\n}\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileRule(tc.rule)
			if err != nil {
				t.Fatalf("CompileRule: %v", err)
			}
			if got != tc.want {
				t.Fatalf("CompileRule() =\n%q\nwant\n%q", got, tc.want)
			}
			if !strings.Contains(tc.want, "require") && strings.Contains(got, "require") {
				t.Fatalf("CompileRule() unexpectedly emitted a require line:\n%q", got)
			}
		})
	}
}
