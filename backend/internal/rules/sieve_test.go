package rules

import (
	"strings"
	"testing"
)

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
