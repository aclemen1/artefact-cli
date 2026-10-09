package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func findTitles(t *testing.T, s *Store, q string, o FindOptions) ([]string, *FindResult) {
	t.Helper()
	r, err := s.Find(q, o)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []string
	for _, h := range r.Hits {
		out = append(out, h.Title)
	}
	return out, r
}

func TestFindGrammar(t *testing.T) {
	s := newStore(t)
	put(t, s, "Bail à loyer signé, garantie de loyer trois mois", PutOptions{Sphere: "perso", Filename: "bail.txt", Title: "Bail", Date: "2023-02-06"})
	put(t, s, "Brouillon du bail à loyer", PutOptions{Sphere: "perso", Filename: "brouillon.txt", Title: "Brouillon bail", Date: "2023-01-10"})
	put(t, s, "Décompte de prestations Assura", PutOptions{Sphere: "perso", Filename: "assura.txt", Title: "Décompte Assura", Date: "2025-04-01"})
	c := put(t, s, "Décompte CSS assurance", PutOptions{Sphere: "perso", Filename: "css.txt", Title: "Décompte CSS", Date: "2021-06-01"})
	s.Tag(c.URI, []string{"anna"}, nil)
	off := false
	o := FindOptions{Semantic: &off}

	cases := []struct {
		q    string
		want []string
	}{
		{`"bail à loyer" -brouillon`, []string{"Bail"}},
		{`assura OR css`, []string{"Décompte Assura", "Décompte CSS"}},
		{`title:décompte after:2024`, []string{"Décompte Assura"}},
		{`décompte year:2021`, []string{"Décompte CSS"}},
		{`décompte -tag:anna`, []string{"Décompte Assura"}},
		{`tag:anna`, []string{"Décompte CSS"}},
		{`loyer AROUND(3) garantie`, []string{"Bail"}},
		{`text:prestations`, []string{"Décompte Assura"}},
	}
	for _, tc := range cases {
		got, _ := findTitles(t, s, tc.q, o)
		if strings.Join(sorted(got), "|") != strings.Join(sorted(tc.want), "|") {
			t.Errorf("%s: got %v, want %v", tc.q, got, tc.want)
		}
	}
	got, r := findTitles(t, s, "prestattions", o)
	if r.Corrected != "prestations" || len(got) != 1 {
		t.Errorf("typo: corrected %q, hits %v", r.Corrected, got)
	}
	if _, err := s.Find(`"open`, o); err == nil || !strings.Contains(err.Error(), "Example") {
		t.Errorf("bad query: %v", err)
	}
}

func sorted(l []string) []string {
	out := append([]string(nil), l...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestFindByMeaning runs the search with a fake qmd that finds, by meaning,
// a text that has none of the words.
func TestFindByMeaning(t *testing.T) {
	s := newStore(t)
	fakeLector(t, s)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	near := put(t, s, "jugement du tribunal sur la consignation", PutOptions{Sphere: "perso", Filename: "jugement.pdf", Mime: "application/pdf", Title: "Jugement"})
	put(t, s, "litige régie", PutOptions{Sphere: "perso", Filename: "note.txt", Title: "Note litige régie"})
	far := put(t, s, "recette de cuisine", PutOptions{Sphere: "perso", Filename: "recette.pdf", Mime: "application/pdf", Title: "Recette"})
	for _, r := range []*PutResult{near, far} {
		if _, err := s.Render(r.URI, false); err != nil {
			t.Fatal(err)
		}
	}
	nearRef, _ := s.Resolve(near.URI)
	farRef, _ := s.Resolve(far.URI)
	script := filepath.Join(t.TempDir(), "qmd")
	body := `#!/bin/sh
case "$3" in
vsearch) printf '[{"score":0.8,"file":"qmd://perso/` + nearRef.Row.Path + `/extensions/artefact/renditions/v1.md","snippet":"@@ -1 @@\\nJUGEMENT DU TRIBUNAL"},{"score":0.3,"file":"qmd://perso/` + farRef.Row.Path + `/extensions/artefact/renditions/v1.md","snippet":"x"}]' ;;
update) echo "Indexed: 2 new, 0 updated, 0 unchanged, 0 removed" ;;
esac
`
	os.WriteFile(script, []byte(body), 0o755)
	s.Cfg.Search.Qmd = []string{script}
	if _, err := s.RefreshSemantic(t.Context(), "perso"); err != nil {
		t.Fatal(err)
	}
	if !s.SemanticReady("perso") {
		t.Fatal("index by meaning not ready")
	}
	r, err := s.Find("litige régie", FindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, h := range r.Hits {
		via[h.Title] = h.Via
	}
	if !r.Semantic || via["Note litige régie"] != "words" || via["Jugement"] != "meaning" || via["Recette"] != "" {
		t.Fatalf("by meaning: %+v", via)
	}
	// A phrase stays strict: the text close in meaning lacks it.
	r, _ = s.Find(`"litige régie"`, FindOptions{})
	if len(r.Hits) != 1 || r.Hits[0].Title != "Note litige régie" {
		t.Errorf("phrase with meaning: %+v", r.Hits)
	}
	// Exclusions and filters apply to what meaning finds.
	r, _ = s.Find(`litige régie -jugement`, FindOptions{})
	for _, h := range r.Hits {
		if h.Title == "Jugement" {
			t.Error("an excluded word let a text found by meaning through")
		}
	}
}
