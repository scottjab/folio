package index

import (
	"strings"
)

// Query is a user's search string translated into something FTS5 will accept.
//
// The translation exists because raw user input and FTS5's query syntax are a
// bad combination: a stray quote, a bare "AND", a hyphen, or a colon all mean
// something to FTS5, and a search box that throws a syntax error at you for
// typing an apostrophe is broken. Every user token therefore reaches FTS5 as a
// quoted string literal, and only the operators we deliberately support survive
// as operators.
type Query struct {
	// Raw is what the user typed, kept for display and logging.
	Raw string
	// Match is the FTS5 MATCH expression, or "" when the query has no positive
	// constraint. An empty Match means "every note", ordered by recency.
	Match string
	// Exclude is an FTS5 MATCH expression whose hits are removed from the
	// result. It exists because FTS5's NOT is a binary operator and cannot
	// express a query that is nothing but exclusions.
	Exclude string
}

// IsEmpty reports whether the query constrains nothing at all.
func (q Query) IsEmpty() bool { return q.Match == "" && q.Exclude == "" }

// fieldColumns maps the field prefixes a user can type onto FTS5 columns. The
// left side is what people actually write; "tag:" is far more natural than the
// column's real name.
var fieldColumns = map[string]string{
	"tag":   "tags",
	"tags":  "tags",
	"path":  "path",
	"file":  "path",
	"title": "title",
	"body":  "body",
	"text":  "body",
}

// ParseQuery translates a search string into a Query. It never fails: bad input
// degrades into a literal search for that input.
//
// Supported syntax:
//
//	term            a word
//	"two words"     a phrase
//	term*           a prefix match
//	tag:go          restrict to a column (tag, path, title, body)
//	-term           exclude
//	-tag:draft      exclude by column
//	a OR b          alternation; uppercase only, so "or" stays a word
func ParseQuery(raw string) Query {
	q := Query{Raw: raw}
	var positive, negative []string

	for _, tok := range tokenize(raw) {
		if tok.text == "" {
			continue
		}
		if tok.text == "OR" && !tok.quoted {
			// Only meaningful between two terms; a leading or trailing OR is
			// dropped rather than producing a syntax error.
			if len(positive) > 0 {
				positive = append(positive, "OR")
			}
			continue
		}
		expr := tok.compile()
		if tok.negated {
			negative = append(negative, expr)
		} else {
			positive = append(positive, expr)
		}
	}

	positive = trimDanglingOr(positive)
	q.Match = strings.Join(positive, " ")
	q.Exclude = strings.Join(negative, " OR ")
	return q
}

// token is one unit of user input on its way to becoming FTS5 syntax.
type token struct {
	text    string
	column  string // FTS5 column to restrict to, or ""
	prefix  bool   // trailing * in the user's input
	negated bool
	quoted  bool // the user wrote it in quotes, so it is a phrase, never an operator
}

// compile renders the token as FTS5. The text always becomes a quoted string
// literal, which is what makes arbitrary input safe.
func (t token) compile() string {
	var b strings.Builder
	if t.column != "" {
		b.WriteString(t.column)
		b.WriteByte(':')
	}
	b.WriteByte('"')
	// Doubling escapes a quote for FTS5; stripping control characters is not
	// cosmetic. SQLite treats an embedded NUL as the end of the string, so a
	// pasted "a\x00b" would reach the engine as an unterminated literal.
	b.WriteString(strings.ReplaceAll(stripControl(t.text), `"`, `""`))
	b.WriteByte('"')
	if t.prefix {
		b.WriteByte('*')
	}
	return b.String()
}

// stripControl removes NUL and other control characters, which SQLite's string
// literals cannot carry.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// tokenize splits input on whitespace, keeping double-quoted runs together. An
// unterminated quote simply runs to the end of the input.
func tokenize(s string) []token {
	var out []token
	var cur strings.Builder
	inQuotes, quoted := false, false

	flush := func() {
		text := cur.String()
		cur.Reset()
		if text == "" && !quoted {
			quoted = false
			return
		}
		out = append(out, classify(text, quoted))
		quoted = false
	}

	for _, r := range s {
		switch {
		case r == '"':
			if inQuotes {
				inQuotes = false
				flush()
			} else {
				// A quote mid-token ends the current token and starts a phrase,
				// so `say"hi` is two tokens rather than a syntax error.
				if cur.Len() > 0 {
					flush()
				}
				inQuotes, quoted = true, true
			}
		case !inQuotes && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// classify turns raw token text into a token, pulling off a leading "-", a
// "field:" prefix, and a trailing "*".
func classify(text string, quoted bool) token {
	t := token{quoted: quoted}

	if !quoted {
		if rest, found := strings.CutPrefix(text, "-"); found {
			// A lone "-" is not a negation of anything.
			if rest == "" {
				return token{}
			}
			t.negated, text = true, rest
		}
		if field, value, found := strings.Cut(text, ":"); found {
			if col, ok := fieldColumns[strings.ToLower(field)]; ok && value != "" {
				t.column, text = col, value
			}
		}
		if rest, found := strings.CutSuffix(text, "*"); found && rest != "" {
			t.prefix, text = true, rest
		}
	}

	t.text = text
	return t
}

// trimDanglingOr removes an OR that ended up at either end of the expression,
// which happens when the user's alternation had nothing on one side.
func trimDanglingOr(terms []string) []string {
	for len(terms) > 0 && terms[0] == "OR" {
		terms = terms[1:]
	}
	for len(terms) > 0 && terms[len(terms)-1] == "OR" {
		terms = terms[:len(terms)-1]
	}
	return terms
}
