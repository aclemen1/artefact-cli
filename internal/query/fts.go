package query

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var columns = map[string]string{"title": "title", "file": "filename", "text": "body", "card": "meta"}

// FTS writes a term in SQLite FTS5 syntax: a word matches the start of
// words, a phrase matches whole words in order.
func FTS(t Term) string {
	var s string
	if t.Phrase {
		s = `"` + t.Text + `"`
	} else {
		s = `"` + t.Text + `"*`
	}
	if c, ok := columns[t.Field]; ok {
		s = c + " : " + s
	}
	return s
}

// Positive is the FTS5 expression every match must satisfy; only, when not
// empty, keeps the strict parts (phrases, fields, OR, AROUND).
func (q *Query) Positive(strictOnly bool) string {
	var parts []string
	for _, g := range q.Groups {
		if strictOnly && len(g) == 1 && !g[0].Phrase && g[0].Field == "" {
			continue
		}
		alts := make([]string, len(g))
		for i, t := range g {
			alts[i] = FTS(t)
		}
		if len(alts) == 1 {
			parts = append(parts, alts[0])
		} else {
			parts = append(parts, "("+strings.Join(alts, " OR ")+")")
		}
	}
	for _, n := range q.Nears {
		a, b := n.A, n.B
		a.Field, b.Field = "", ""
		parts = append(parts, "NEAR("+FTS(a)+" "+FTS(b)+", "+itoa(n.Distance)+")")
	}
	return strings.Join(parts, " AND ")
}

// Any is an expression that matches when any plain word matches: the lexical
// side of a semantic search, which ranks rather than requires.
func (q *Query) Any() string {
	var parts []string
	for _, g := range q.Groups {
		for _, t := range g {
			parts = append(parts, FTS(t))
		}
	}
	return strings.Join(parts, " OR ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Fold lowers a word and strips its accents, as the index stores words.
func Fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return strings.ToLower(s)
	}
	return out
}

// Distance is the Levenshtein distance between two words.
func Distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
