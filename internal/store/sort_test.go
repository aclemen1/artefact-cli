package store

import (
	"testing"

	"github.com/aclemen1/artefact-cli/internal/index"
)

func titles(items []ListItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func TestSortOrders(t *testing.T) {
	s := newStore(t)
	put(t, s, "bbbb facture", PutOptions{Filename: "b.txt", Date: "2024-05-01"})
	put(t, s, "a", PutOptions{Filename: "a.txt", Date: "2026-01-01"})
	put(t, s, "cc facture facture", PutOptions{Filename: "c.txt", Date: "2025-03-01"})
	put(t, s, "d", PutOptions{Filename: "d.txt", Date: "2026-06-01"})
	cases := map[string][]string{
		"date":       {"d", "a", "c", "b"},
		"date:asc":   {"b", "c", "a", "d"},
		"title":      {"a", "b", "c", "d"},
		"title:desc": {"d", "c", "b", "a"},
		"size:asc":   {"d", "a", "b", "c"},
	}
	for spec, want := range cases {
		o, err := ParseSort(spec)
		if err != nil {
			t.Fatal(err)
		}
		items, err := s.List("perso", index.Filter{SortKey: o.Key, SortDesc: o.Desc})
		if err != nil {
			t.Fatal(err)
		}
		got := titles(items)
		if len(got) != len(want) {
			t.Fatalf("%s: %v", spec, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", spec, got, want)
				break
			}
		}
	}
	if _, err := ParseSort("colour"); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := s.List("", index.Filter{SortKey: "relevance"}); err == nil {
		t.Error("relevance accepted by ls")
	}
	hits, err := s.Search("facture", "", 10, "all", Sort{Key: "date", Desc: false})
	if err != nil || len(hits) != 2 || hits[0].Title != "b" {
		t.Fatalf("search by date: %v %v", hits, err)
	}
}
