package store

import (
	"errors"
	"strings"
	"unicode"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/query"
)

// FindOptions tune a search.
type FindOptions struct {
	Sphere   string
	Limit    int    // default 20; 0 or less for the default
	State    string // active, archived or all (default); is: in the query wins
	Sort     Sort
	Semantic *bool // nil: when the sphere has a semantic index
}

// FindResult is a search's answer.
type FindResult struct {
	Query     string      `json:"query"`
	Corrected string      `json:"corrected,omitempty"` // the query as searched, when words were corrected
	Semantic  bool        `json:"semantic"`            // meaning took part in the ranking
	Notes     []string    `json:"notes,omitempty"`
	Hits      []SearchHit `json:"hits"`
}

// rrfK damps the weight of the first ranks when two rankings merge.
const rrfK = 60

// semanticCandidates is how many nearest texts the semantic side brings.
const semanticCandidates = 40

// minSemanticScore drops the texts that are only vaguely close.
const minSemanticScore = 0.55

// Find searches the granted spheres with the query grammar (package query):
// the words rank by where they appear and, when the sphere has a semantic
// index, by meaning; phrases, fields, exclusions and filters are strict.
func (s *Store) Find(q string, o FindOptions) (*FindResult, error) {
	if strings.TrimSpace(q) == "" {
		return nil, userErr("search needs words or filters. Example: artefact search décompte assura type:pdf after:2024")
	}
	parsed, err := query.Parse(q)
	if err != nil {
		var qe *query.Error
		if errors.As(err, &qe) {
			return nil, userErr("%s", qe.Error())
		}
		return nil, err
	}
	spheres := s.Spheres
	sphere := o.Sphere
	if parsed.Sphere != "" {
		sphere = parsed.Sphere
	}
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 20
	}
	res := &FindResult{Query: q, Hits: []SearchHit{}}
	if parsed.HasText() {
		if corrected := s.correct(parsed, spheres); corrected != "" {
			res.Corrected = corrected
		}
	}
	for _, sp := range spheres {
		hits, semantic, notes, err := s.findIn(sp, parsed, o)
		if err != nil {
			return nil, err
		}
		res.Semantic = res.Semantic || semantic
		res.Notes = append(res.Notes, notes...)
		res.Hits = append(res.Hits, hits...)
	}
	order := o.Sort
	if !parsed.HasText() && order.Key == "" {
		order = Sort{Key: "deposited", Desc: true}
	}
	sortHits(res.Hits, order.orDefault("relevance"))
	if len(res.Hits) > limit {
		res.Hits = res.Hits[:limit]
	}
	return res, nil
}

func (s *Store) findIn(sp string, q *query.Query, o FindOptions) ([]SearchHit, bool, []string, error) {
	x, err := s.index(sp)
	if err != nil {
		return nil, false, nil, err
	}
	narrow := index.Narrow{Types: q.Types, Plans: q.Plans, Tags: q.Tags, NotTags: q.NotTags,
		After: q.After, Before: q.Before, Is: q.Is, IsNot: q.IsNot}
	if !slicesHasAny(q.Is, "archived", "active") && !slicesHasAny(q.IsNot, "archived", "active") {
		switch o.State {
		case "active":
			narrow.Is = append(narrow.Is, "active")
		case "archived":
			narrow.Is = append(narrow.Is, "archived")
		}
	}
	if len(q.Classes) > 0 {
		classes, err := s.resolveClasses(sp, q.Classes)
		if err != nil {
			return nil, false, nil, err
		}
		if len(classes) == 0 {
			return nil, false, nil, nil // no such class in this sphere
		}
		narrow.Classes = classes
	}
	allowed, err := x.Narrowed(narrow)
	if err != nil {
		return nil, false, nil, err
	}
	item := func(r index.Row) ListItem { return ListItem{Row: r, Sphere: sp, URI: URI(sp, r.ID)} }
	if !q.HasText() {
		out := make([]SearchHit, 0, len(allowed))
		for _, r := range allowed {
			out = append(out, SearchHit{ListItem: item(r), Via: "filter"})
		}
		return out, false, nil, nil
	}

	excluded := map[string]bool{}
	for _, t := range q.Not {
		hits, err := x.Match(query.FTS(t), 0)
		if err != nil {
			return nil, false, nil, ftsErr(err)
		}
		for _, h := range hits {
			excluded[h.ID] = true
		}
	}
	lex, err := x.Match(q.Positive(false), 0)
	if err != nil {
		return nil, false, nil, ftsErr(err)
	}
	type cand struct {
		row          index.Row
		lexRank      int
		semRank      int
		snippet      string
		bySem, byLex bool
	}
	cands := map[string]*cand{}
	for i, h := range lex {
		r, ok := allowed[h.ID]
		if !ok || excluded[h.ID] {
			continue
		}
		cands[h.ID] = &cand{row: r, lexRank: i, snippet: h.Snippet, byLex: true}
	}

	var notes []string
	semantic := false
	useSem := o.Semantic == nil || *o.Semantic
	if free := q.Free(); useSem && free != "" && s.semanticReady(sp) {
		paths, err := x.Paths()
		if err != nil {
			return nil, false, nil, err
		}
		sem, err := s.semanticSearch(sp, free, semanticCandidates, paths)
		if err != nil {
			notes = append(notes, "search by meaning unavailable in "+sp+": "+err.Error())
		} else {
			semantic = true
			// Phrases, fields, OR and AROUND stay strict: a text close in
			// meaning must still satisfy them.
			var strict map[string]bool
			if q.Strict() {
				strict = map[string]bool{}
				if expr := q.Positive(true); expr != "" {
					hits, err := x.Match(expr, 0)
					if err != nil {
						return nil, false, nil, ftsErr(err)
					}
					for _, h := range hits {
						strict[h.ID] = true
					}
				}
			}
			for id, h := range sem {
				if h.score < minSemanticScore {
					continue
				}
				r, ok := allowed[id]
				if !ok || excluded[id] || (strict != nil && !strict[id]) {
					continue
				}
				c := cands[id]
				if c == nil {
					c = &cand{row: r, lexRank: -1, snippet: h.snippet}
					cands[id] = c
				}
				c.bySem, c.semRank = true, h.rank
			}
		}
	} else if useSem && free != "" && o.Semantic != nil && !s.semanticReady(sp) {
		notes = append(notes, "no index by meaning for "+sp+": artefact reindex --semantic")
	}

	out := make([]SearchHit, 0, len(cands))
	for _, c := range cands {
		score := 0.0
		via := []string{}
		if c.byLex {
			score += 1 / float64(rrfK+c.lexRank)
			via = append(via, "words")
		}
		if c.bySem {
			score += 1 / float64(rrfK+c.semRank)
			via = append(via, "meaning")
		}
		// Lower is better, as for bm25.
		out = append(out, SearchHit{ListItem: item(c.row), Snippet: c.snippet, Score: -score, Via: strings.Join(via, "+")})
	}
	return out, semantic, notes, nil
}

func ftsErr(err error) error {
	return userErr("the search cannot read this query (%v). Quote the words that hold signs: \"e-mail\"", err)
}

func slicesHasAny(l []string, xs ...string) bool {
	for _, a := range l {
		for _, b := range xs {
			if a == b {
				return true
			}
		}
	}
	return false
}

// resolveClasses turns class: values into plan:notation pairs of sphere: a
// notation (4.2), plan:notation, or the words of a class label (banque).
func (s *Store) resolveClasses(sphere string, values []string) ([]string, error) {
	plans, err := s.Plans().List()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range values {
		if isNotation(v) || strings.Contains(v, ":") {
			out = append(out, v)
			continue
		}
		want := query.Fold(v)
		for _, p := range plans {
			if p.Sphere != sphere {
				continue
			}
			var walk func([]*classify.Node)
			walk = func(ns []*classify.Node) {
				for _, n := range ns {
					if strings.Contains(query.Fold(n.Label), want) {
						out = append(out, p.ID+":"+n.Notation)
					}
					walk(n.Children)
				}
			}
			walk(p.Nodes)
		}
	}
	return out, nil
}

func isNotation(v string) bool {
	for _, r := range v {
		if !unicode.IsDigit(r) && r != '.' {
			return false
		}
	}
	return v != ""
}

// correct replaces, in q, the plain words that match nothing by the closest
// indexed word, and returns the corrected query, or "" when none changed.
func (s *Store) correct(q *query.Query, spheres []string) string {
	changed := false
	for gi := range q.Groups {
		for ti := range q.Groups[gi] {
			t := &q.Groups[gi][ti]
			if t.Phrase || len([]rune(t.Text)) < 5 {
				continue
			}
			found, best, bestDocs := false, "", 0
			w := query.Fold(t.Text)
			for _, sp := range spheres {
				x, err := s.index(sp)
				if err != nil {
					continue
				}
				if hits, err := x.Match(query.FTS(query.Term{Text: t.Text}), 1); err == nil && len(hits) > 0 {
					found = true
					break
				}
				vocab, err := x.Vocabulary(string([]rune(w)[:1]))
				if err != nil {
					continue
				}
				maxDist := 1
				if len([]rune(w)) >= 8 {
					maxDist = 2
				}
				for term, docs := range vocab {
					d := query.Distance(w, term)
					if d == 0 || d > maxDist {
						continue
					}
					if best == "" || d < query.Distance(w, best) || (d == query.Distance(w, best) && docs > bestDocs) {
						best, bestDocs = term, docs
					}
				}
			}
			if !found && best != "" {
				t.Text = best
				changed = true
			}
		}
	}
	if !changed {
		return ""
	}
	var words []string
	for _, g := range q.Groups {
		var alts []string
		for _, t := range g {
			alts = append(alts, t.Text)
		}
		words = append(words, strings.Join(alts, " OR "))
	}
	return strings.Join(words, " ")
}
