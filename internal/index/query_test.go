package index_test

import (
	"strings"
	"testing"

	"github.com/scottjab/folio/internal/index"
)

func TestParseQuery(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantMatch   string
		wantExclude string
	}{
		{"single term", "hello", `"hello"`, ""},
		{"implicit AND", "hello world", `"hello" "world"`, ""},
		{"phrase", `"exact phrase"`, `"exact phrase"`, ""},
		{"phrase plus term", `go "exact phrase"`, `"go" "exact phrase"`, ""},

		{"tag filter", "tag:go", `tags:"go"`, ""},
		{"tags alias", "tags:go", `tags:"go"`, ""},
		{"path filter", "path:Daily", `path:"Daily"`, ""},
		{"title filter", "title:folio", `title:"folio"`, ""},
		{"unknown field is a literal term", "foo:bar", `"foo:bar"`, ""},

		{"prefix", "foo*", `"foo"*`, ""},
		{"prefix inside a field", "tag:go*", `tags:"go"*`, ""},

		{"negation", "hello -draft", `"hello"`, `"draft"`},
		{"negated field", "hello -tag:draft", `"hello"`, `tags:"draft"`},
		{"only negation", "-draft", "", `"draft"`},
		{"multiple negations", "-a -b", "", `"a" OR "b"`},

		{"explicit OR", "cats OR dogs", `"cats" OR "dogs"`, ""},
		{"or is case sensitive", "cats or dogs", `"cats" "or" "dogs"`, ""},

		{"empty", "", "", ""},
		{"whitespace only", "   \t ", "", ""},
		{"lone dash", "-", "", ""},
		{"lone colon", ":", `":"`, ""},

		// Raw input must never reach FTS5 as syntax. Everything gets quoted.
		{"unbalanced quote", `hello "world`, `"hello" "world"`, ""},
		{"embedded quote", `say "hi" there`, `"say" "hi" "there"`, ""},
		{"fts operators are literals", "AND NOT (a)", `"AND" "NOT" "(a)"`, ""},
		{"sql injection is inert", `'; DROP TABLE notes;--`, `"';" "DROP" "TABLE" "notes;--"`, ""},
		{"caret and braces", "^{a b}", `"^{a" "b}"`, ""},
		{"stray quote starts a phrase", `say"hi`, `"say" "hi"`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := index.ParseQuery(tt.raw)
			if q.Match != tt.wantMatch {
				t.Errorf("Match = %q, want %q", q.Match, tt.wantMatch)
			}
			if q.Exclude != tt.wantExclude {
				t.Errorf("Exclude = %q, want %q", q.Exclude, tt.wantExclude)
			}
		})
	}
}

func TestParseQueryAlwaysBalancesQuotes(t *testing.T) {
	// Whatever the user types, every token in the output must be a complete
	// quoted string literal. TestParseQueryProducesValidFTS5 proves this against
	// the real engine; this is the cheap structural check.
	for _, raw := range []string{`a"b`, `"unclosed`, `""""`, `-"x`, `tag:"go`, `a""b`} {
		q := index.ParseQuery(raw)
		for _, expr := range []string{q.Match, q.Exclude} {
			if strings.Count(expr, `"`)%2 != 0 {
				t.Errorf("ParseQuery(%q) produced %q with an odd number of quotes", raw, expr)
			}
		}
	}
}

func TestQueryIsEmpty(t *testing.T) {
	if !index.ParseQuery("").IsEmpty() {
		t.Error("empty input should produce an empty query")
	}
	if index.ParseQuery("hello").IsEmpty() {
		t.Error("a term is not an empty query")
	}
	if index.ParseQuery("-draft").IsEmpty() {
		t.Error("a negation alone is still a query, it just has no positive match")
	}
}
