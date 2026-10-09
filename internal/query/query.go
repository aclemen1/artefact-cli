// Package query reads the search grammar of artefact, modelled on Google's:
// words, "exact phrases", -exclusions, OR, AROUND(n), field:value filters.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Term is a word or a phrase to match.
type Term struct {
	Text   string // one word, or the words of a phrase
	Phrase bool   // "…": these words, in this order, whole
	Field  string // "" anywhere; title, file, text, card
}

// Near asks for two terms within Distance words of each other.
type Near struct {
	A, B     Term
	Distance int
}

// Query is a parsed search.
type Query struct {
	// Groups must all match; the terms of a group are alternatives (OR).
	Groups [][]Term
	Nears  []Near
	// Not excludes the artefacts that match any of these terms.
	Not []Term

	Types   []string // pdf, image, text, audio, video, an extension, or a media type; any of them
	Classes []string // notation, plan:notation, or a class label; any of them
	Plans   []string
	Tags    []string
	NotTags []string
	After   string // YYYY-MM-DD, inclusive
	Before  string // YYYY-MM-DD, exclusive
	Sphere  string
	Is      []string // sealed, archived, active, pending, orphaned, classified, unclassified
	IsNot   []string
}

// Fields maps the field names a user may type to the indexed field.
var Fields = map[string]string{
	"title": "title", "intitle": "title", "titre": "title",
	"file": "file", "filename": "file", "fichier": "file",
	"text": "text", "intext": "text", "texte": "text", "body": "text",
	"card": "card", "fiche": "card",
}

var states = map[string]string{
	"sealed": "sealed", "scellé": "sealed", "scelle": "sealed",
	"archived": "archived", "archivé": "archived", "archive": "archived",
	"active": "active", "actif": "active",
	"pending": "pending", "attente": "pending",
	"orphaned": "orphaned", "orphelin": "orphaned",
	"classified": "classified", "classé": "classified", "classe": "classified",
	"unclassified": "unclassified", "nonclassé": "unclassified",
}

// Error is a query that cannot be read; Hint shows the right form.
type Error struct{ Msg, Hint string }

func (e *Error) Error() string { return e.Msg + ". " + e.Hint }

// Parse reads q. Plain words match the start of a word, accents and case
// ignored; "quoted" words match whole.
func Parse(q string) (*Query, error) {
	toks, err := tokenize(q)
	if err != nil {
		return nil, err
	}
	out := &Query{}
	var group []Term
	pendingOr := false
	flush := func() {
		if len(group) > 0 {
			out.Groups = append(out.Groups, group)
			group = nil
		}
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !t.quoted && (t.text == "OR" || t.text == "|") {
			if len(group) == 0 {
				return nil, &Error{"OR needs a term on each side", `Example: assura OR css`}
			}
			pendingOr = true
			continue
		}
		if !t.quoted && strings.HasPrefix(t.text, "AROUND(") {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(t.text, "AROUND("), ")"))
			if err != nil || n < 1 || len(group) != 1 || i+1 >= len(toks) {
				return nil, &Error{"AROUND(n) sits between two terms", `Example: loyer AROUND(5) garantie`}
			}
			a := group[0]
			b, ok, err := termOf(toks[i+1])
			if err != nil || !ok {
				return nil, &Error{"AROUND(n) sits between two terms", `Example: loyer AROUND(5) garantie`}
			}
			out.Nears = append(out.Nears, Near{A: a, B: b, Distance: n})
			group = nil
			i++
			continue
		}
		if !pendingOr {
			flush()
		}
		pendingOr = false
		if !t.quoted && t.neg {
			if f, v, ok := field(t.text); ok {
				switch f {
				case "is":
					s, ok := states[strings.ToLower(v)]
					if !ok {
						return nil, stateErr(v)
					}
					out.IsNot = append(out.IsNot, s)
					continue
				case "tag":
					out.NotTags = append(out.NotTags, strings.ToLower(v))
					continue
				}
			}
		}
		if handled, err := out.filter(t); err != nil {
			return nil, err
		} else if handled {
			continue
		}
		term, ok, err := termOf(t)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if t.neg {
			out.Not = append(out.Not, term)
			continue
		}
		group = append(group, term)
	}
	if pendingOr {
		return nil, &Error{"OR needs a term on each side", `Example: assura OR css`}
	}
	flush()
	return out, nil
}

func stateErr(v string) error {
	return &Error{fmt.Sprintf("is:%s is not a state", v), "States: is:sealed, is:archived, is:active, is:pending, is:orphaned, is:classified, is:unclassified"}
}

// filter takes the field:value tokens that narrow the artefacts.
func (q *Query) filter(t token) (bool, error) {
	if t.quoted || t.neg {
		return false, nil
	}
	f, v, ok := field(t.text)
	if !ok || v == "" {
		return false, nil
	}
	switch f {
	case "type", "ext", "mime":
		q.Types = append(q.Types, strings.ToLower(strings.TrimPrefix(v, ".")))
	case "class", "classe":
		q.Classes = append(q.Classes, v)
	case "plan":
		q.Plans = append(q.Plans, v)
	case "tag":
		q.Tags = append(q.Tags, strings.ToLower(v))
	case "sphere":
		q.Sphere = strings.ToLower(v)
	case "is", "state":
		s, ok := states[strings.ToLower(v)]
		if !ok {
			return false, stateErr(v)
		}
		q.Is = append(q.Is, s)
	case "after", "since", "après", "apres", "depuis":
		d, _, err := dateRange(v)
		if err != nil {
			return false, err
		}
		q.After = maxDate(q.After, d)
	case "before", "avant":
		d, _, err := dateRange(v)
		if err != nil {
			return false, err
		}
		q.Before = minDate(q.Before, d)
	case "year", "année", "annee", "date":
		from, to, err := dateRange(v)
		if err != nil {
			return false, err
		}
		q.After, q.Before = maxDate(q.After, from), minDate(q.Before, to)
	default:
		return false, nil
	}
	return true, nil
}

func maxDate(a, b string) string {
	if a == "" || b > a {
		return b
	}
	return a
}

func minDate(a, b string) string {
	if a == "" || (b != "" && b < a) {
		return b
	}
	return a
}

// dateRange reads YYYY, YYYY-MM or YYYY-MM-DD as [from, to).
func dateRange(v string) (string, string, error) {
	bad := &Error{fmt.Sprintf("%q is not a date", v), "Dates are YYYY, YYYY-MM or YYYY-MM-DD. Example: after:2024-03 before:2025"}
	for _, l := range []struct {
		layout string
		next   func(time.Time) time.Time
	}{
		{"2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }},
		{"2006-01", func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }},
		{"2006", func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }},
	} {
		if t, err := time.Parse(l.layout, v); err == nil {
			return t.Format("2006-01-02"), l.next(t).Format("2006-01-02"), nil
		}
	}
	return "", "", bad
}

// field splits name:value when name is a known operator.
func field(s string) (string, string, bool) {
	name, v, ok := strings.Cut(s, ":")
	if !ok {
		return "", "", false
	}
	name = strings.ToLower(name)
	switch name {
	case "type", "ext", "mime", "class", "classe", "plan", "tag", "sphere", "is", "state",
		"after", "since", "après", "apres", "depuis", "before", "avant", "year", "année", "annee", "date":
		return name, strings.Trim(v, `"`), true
	}
	if _, ok := Fields[name]; ok {
		return name, v, true
	}
	return "", "", false
}

// termOf turns a token into a term; ok is false when it holds no word.
func termOf(t token) (Term, bool, error) {
	text, fieldName := t.text, ""
	if f, v, ok := field(text); ok && !t.quoted {
		mapped, isText := Fields[f]
		if !isText {
			return Term{}, false, &Error{fmt.Sprintf("%s: cannot be excluded or combined", f), "Exclude words or tags only. Example: -brouillon -tag:anna"}
		}
		fieldName, text = mapped, v
	}
	phrase := t.quoted
	if strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) && len(text) >= 2 {
		text, phrase = text[1:len(text)-1], true
	}
	words := Words(text)
	if len(words) == 0 {
		return Term{}, false, nil
	}
	if !phrase && len(words) > 1 {
		// "l'assurance" or "e-mail": the pieces form a phrase, as FTS5 reads them.
		phrase = true
	}
	return Term{Text: strings.Join(words, " "), Phrase: phrase, Field: fieldName}, true, nil
}

// Words splits text as the index does: letters and digits, the rest separates.
func Words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

type token struct {
	text   string
	quoted bool
	neg    bool
}

// tokenize splits on spaces, keeping "quoted phrases" and field:"quoted values" whole.
func tokenize(q string) ([]token, error) {
	var out []token
	rs := []rune(strings.TrimSpace(q))
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		neg := false
		if rs[i] == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) {
			neg = true
			i++
		}
		if rs[i] == '"' {
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if j >= len(rs) {
				return nil, &Error{"a quote is not closed", `Example: "bail à loyer"`}
			}
			out = append(out, token{text: string(rs[i+1 : j]), quoted: true, neg: neg})
			i = j + 1
			continue
		}
		j := i
		inQuote := false
		for j < len(rs) && (inQuote || !unicode.IsSpace(rs[j])) {
			if rs[j] == '"' {
				inQuote = !inQuote
			}
			j++
		}
		if inQuote {
			return nil, &Error{"a quote is not closed", `Example: title:"bail à loyer"`}
		}
		out = append(out, token{text: string(rs[i:j]), neg: neg})
		i = j
	}
	return out, nil
}

// Free is the text that carries meaning, for a semantic search: the words
// and phrases to find, without operators, fields or exclusions.
func (q *Query) Free() string {
	var parts []string
	for _, g := range q.Groups {
		for _, t := range g {
			parts = append(parts, t.Text)
		}
	}
	for _, n := range q.Nears {
		parts = append(parts, n.A.Text, n.B.Text)
	}
	return strings.Join(parts, " ")
}

// Strict reports whether some terms must match as written: phrases, fields
// and AROUND. Plain words only rank; their meaning may find more.
func (q *Query) Strict() bool {
	if len(q.Nears) > 0 {
		return true
	}
	for _, g := range q.Groups {
		for _, t := range g {
			if t.Phrase || t.Field != "" || len(g) > 1 {
				return true
			}
		}
	}
	return false
}

// HasText reports whether q has words to find, besides filters.
func (q *Query) HasText() bool { return len(q.Groups) > 0 || len(q.Nears) > 0 }
