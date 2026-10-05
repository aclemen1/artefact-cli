package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aclemen1/artefact-cli/internal/index"
)

const planYAML = `id: household
title: Household records
sphere: perso
nodes:
  - notation: "3"
    label: Health
    scope: Medical records, prescriptions, insurance.
    retention: { keep: 10y, then: review }
    children:
      - notation: "3.1"
        label: Prescriptions
        scope: Doctor's prescriptions.
      - notation: "3.2"
        label: Insurance
        scope: Health insurance statements and claims.
        retention: { keep: 5y, then: destroy }
  - notation: "4"
    label: Housing
    scope: Rent, energy, repairs.
    rules:
      - { filename: "(?i)loyer" }
`

// fakeJev answers like /v1/systemone: it picks the candidate whose
// description contains a word of the state, with the given confidence.
func fakeJev(t *testing.T, confidence float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/v1/systemone" {
			http.Error(w, "bad request", 400)
			return
		}
		q := body.Questions["node"]
		choice := "none"
		state := strings.ToLower(body.State)
		for k, d := range q.Criteria {
			if k == "none" {
				continue
			}
			for _, word := range strings.Fields(strings.ToLower(strings.Trim(d, "."))) {
				word = strings.Trim(word, ",.:")
				if len(word) > 5 && strings.Contains(state, word) {
					choice = k
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"node": map[string]any{
			"type": "choice", "choice": choice, "confidence": confidence, "probabilities": map[string]float64{choice: confidence},
		}}})
	}))
}

func withPlan(t *testing.T, jevURL string) *Store {
	t.Helper()
	s := newStore(t)
	if _, err := s.Plans().Save([]byte(planYAML), false); err != nil {
		t.Fatal(err)
	}
	s.Cfg.Classify.Deciders.OpenJev.URL = jevURL
	return s
}

func TestClassifyWalk(t *testing.T) {
	jev := fakeJev(t, 0.95)
	defer jev.Close()
	s := withPlan(t, jev.URL)
	ctx := context.Background()

	ins := put(t, s, "Assura: statements of health insurance claims for 2026", PutOptions{Filename: "decompte.txt", Title: "Décompte Assura", Date: "2020-03-01"})
	r, err := s.Classify(ctx, ins.ID, "household", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "classified" || r.Node != "3.2" || r.By != "decider:openjev" || len(r.Steps) != 2 {
		t.Fatalf("walk: %+v", r)
	}
	rent := put(t, s, "x", PutOptions{Filename: "loyer-octobre.pdf"})
	if r, _ := s.Classify(ctx, rent.ID, "household", ""); r.Node != "4" || r.By != "rule:4" {
		t.Errorf("rule: %+v", r)
	}
	other := put(t, s, "nothing related", PutOptions{Filename: "note.txt"})
	if r, _ := s.Classify(ctx, other.ID, "household", ""); r.Status != "pending" || r.Node != "" {
		t.Errorf("none at the top must be pending: %+v", r)
	}

	// A human decision is kept.
	if _, err := s.SetClass(ins.ID, "household", "3.1", "alain"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Classify(ctx, ins.ID, "household", ""); r.Status != "kept_human" || r.Node != "3.1" {
		t.Errorf("human decision overwritten: %+v", r)
	}
	if _, err := s.SetClass(ins.ID, "household", "9.9", "alain"); err == nil {
		t.Error("an unknown node must be refused")
	}

	// Filters and reindex.
	if l, _ := s.List("perso", index.Filter{Class: "household:3"}); len(l) != 1 {
		t.Errorf("class 3 and below: %d", len(l))
	}
	if l, _ := s.List("perso", index.Filter{Pending: true}); len(l) != 1 || l[0].ID != other.ID {
		t.Errorf("pending: %v", l)
	}
	if _, err := s.Reindex("perso"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List("perso", index.Filter{Class: "household"}); len(l) != 3 {
		t.Errorf("classes after reindex: %d", len(l))
	}
	st, _, _ := s.Stat([]string{ins.ID}, true)
	if c := st[0].Card.Classes["household"]; c.By != "human:alain" {
		t.Errorf("card class %+v", c)
	}
	last := st[0].Events[len(st[0].Events)-1]
	if last.Type != "classify" {
		t.Errorf("last event %+v", last)
	}
}

func TestClassifyLowConfidenceIsPending(t *testing.T) {
	jev := fakeJev(t, 0.5)
	defer jev.Close()
	s := withPlan(t, jev.URL)
	a := put(t, s, "health insurance claims", PutOptions{Filename: "a.txt"})
	r, err := s.Classify(context.Background(), a.ID, "household", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "pending" || r.Node != "" {
		t.Errorf("low confidence: %+v", r)
	}
}

func TestSealedArtefactGivesItsCardOnly(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		seen = body["state"].(string)
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"node": map[string]any{"choice": "none", "confidence": 0.9}}})
	}))
	defer srv.Close()
	s := sealedStore(t)
	s.Plans().Save([]byte(planYAML), false)
	s.Cfg.Classify.Deciders.OpenJev.URL = srv.URL
	a := put(t, s, "TOPSECRET health insurance", PutOptions{Filename: "s.txt", Title: "Carte", Seal: true})
	if _, err := s.Classify(context.Background(), a.ID, "household", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "TOPSECRET") || !strings.Contains(seen, "Carte") {
		t.Errorf("the decider saw %q", seen)
	}
}

func TestReview(t *testing.T) {
	s := withPlan(t, "http://127.0.0.1:1")
	old := put(t, s, "a", PutOptions{Filename: "a.txt", Date: "2015-01-10"})
	recent := put(t, s, "b", PutOptions{Filename: "b.txt", Date: time.Now().Format("2006-01-02")})
	s.SetClass(old.ID, "household", "3.2", "alain")
	s.SetClass(recent.ID, "household", "3.2", "alain")
	due, err := s.Review("", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].URI != old.URI || due[0].Then != "destroy" || due[0].Due != "2020-01-10" || due[0].RuleAt != "3.2" {
		t.Fatalf("review %+v", due)
	}
	// Inherited rule: 3.1 has none, 3 keeps 10 years.
	s.SetClass(old.ID, "household", "3.1", "alain")
	due, _ = s.Review("", time.Now())
	if len(due) != 1 || due[0].Keep != "10y" || due[0].RuleAt != "3" || due[0].Then != "review" {
		t.Errorf("inherited %+v", due)
	}
	if _, err := s.Resolve(old.ID); err != nil {
		t.Error("review must destroy nothing")
	}
	p, _ := s.Plans().Load("household")
	ttl := p.SKOS("")
	if !strings.Contains(ttl, `skos:broader <urn:artefact:plan:household:3>`) || !strings.Contains(ttl, `art:keep "5y"`) {
		t.Errorf("SKOS:\n%s", ttl)
	}
}

func TestArchive(t *testing.T) {
	s := newStore(t)
	plan := strings.Replace(planYAML, "retention: { keep: 5y, then: destroy }", "retention: { keep: 5y, then: archive }", 1)
	if _, err := s.Plans().Save([]byte(plan), false); err != nil {
		t.Fatal(err)
	}
	old := put(t, s, "old claim", PutOptions{Filename: "a.txt", Date: "2015-01-10"})
	put(t, s, "current", PutOptions{Filename: "b.txt"})
	s.SetClass(old.ID, "household", "3.2", "alain")
	due, _ := s.Review("", time.Now())
	if len(due) != 1 || due[0].Then != "archive" {
		t.Fatalf("due %+v", due)
	}
	done, err := s.ApplyArchive(due)
	if err != nil || done[0].Done != "archived" {
		t.Fatalf("apply %+v %v", done, err)
	}
	if l, _ := s.List("perso", index.Filter{State: "active"}); len(l) != 1 {
		t.Errorf("active: %d", len(l))
	}
	if l, _ := s.List("perso", index.Filter{State: "archived"}); len(l) != 1 || l[0].ID != old.ID {
		t.Errorf("archived: %v", l)
	}
	if again, _ := s.Review("", time.Now()); len(again) != 0 {
		t.Errorf("an archived artefact is not due again: %+v", again)
	}
	if _, err := s.Resolve(old.ID); err != nil {
		t.Error("an archived artefact stays reachable")
	}
	if _, err := s.Archive(old.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List("perso", index.Filter{State: "active"}); len(l) != 2 {
		t.Errorf("after unarchive: %d", len(l))
	}
}

func TestSearch(t *testing.T) {
	s := sealedStore(t)
	a := put(t, s, "Assura SA. Décompte de prestations, franchise 300 CHF, quote-part 10 %.", PutOptions{Filename: "decompte-assura.txt", Title: "Décompte Assura septembre"})
	put(t, s, "Ordonnance du Dr Rochat: Plaquenil 200 mg", PutOptions{Filename: "ordonnance.txt", Sphere: "pro"})
	sealed := put(t, s, "SECRETWORD inside", PutOptions{Filename: "codes.txt", Title: "Codes de secours", Seal: true})
	s.EditCard(a.ID, []string{"dc.creator=Assura SA"}, "")

	hits, err := s.Search("decompte", "", 10, "all")
	if err != nil || len(hits) != 1 || hits[0].ID != a.ID {
		t.Fatalf("accent-free title: %v %v", hits, err)
	}
	if hits, _ := s.Search("franch quote", "", 10, "all"); len(hits) != 1 {
		t.Errorf("prefixes in the content: %v", hits)
	}
	if hits, _ := s.Search("plaquenil", "perso", 10, "all"); len(hits) != 0 {
		t.Errorf("a pro artefact found in perso: %v", hits)
	}
	if hits, _ := s.Search("plaquenil", "", 10, "all"); len(hits) != 1 || hits[0].Sphere != "pro" {
		t.Errorf("across spheres: %v", hits)
	}
	if hits, _ := s.Search("secretword", "", 10, "all"); len(hits) != 0 {
		t.Error("the content of a sealed artefact is searchable")
	}
	if hits, _ := s.Search("secours", "", 10, "all"); len(hits) != 1 || hits[0].ID != sealed.ID {
		t.Errorf("a sealed artefact is found by its card: %v", hits)
	}
	s.Archive(a.ID, true, "")
	if hits, _ := s.Search("assura", "", 10, "active"); len(hits) != 0 {
		t.Errorf("archived in active search: %v", hits)
	}
	if _, err := s.Reindex(""); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search("assura", "", 10, "all"); len(hits) != 1 || !strings.Contains(hits[0].Snippet, "«") {
		t.Errorf("after reindex: %v", hits)
	}
}

func TestWhich(t *testing.T) {
	s := sealedStore(t)
	clear := put(t, s, "facture électricité", PutOptions{Filename: "f.pdf", Holds: []index.Holder{{By: "mnemo", Ref: "C1"}}})
	sealed := put(t, s, "codes 2FA", PutOptions{Filename: "c.txt", Seal: true})
	r, err := s.Which(strings.NewReader("facture électricité"), "")
	if err != nil || !r.Found || r.ID != clear.ID || len(r.Holders) != 1 || r.Holders[0].By != "mnemo" {
		t.Fatalf("clear: %+v %v", r, err)
	}
	if r, _ := s.Which(strings.NewReader("codes 2FA"), "perso"); !r.Found || r.ID != sealed.ID || !r.Sealed {
		t.Errorf("sealed by its clear content: %+v", r)
	}
	if r, _ := s.Which(strings.NewReader("absent"), ""); r.Found {
		t.Errorf("absent: %+v", r)
	}
	if r, _ := s.Which(strings.NewReader("facture électricité"), "pro"); r.Found {
		t.Error("a perso artefact found in pro")
	}
	if l, _ := s.List("", index.Filter{}); len(l) != 2 {
		t.Errorf("which must write nothing: %d artefacts", len(l))
	}
}
