package query

import (
	"errors"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	q, err := Parse(`facture "bail à loyer" -brouillon assura OR css title:décompte type:pdf class:4.2 tag:eve -tag:jeannette after:2024-03 before:2025 is:pending -is:sealed loyer AROUND(5) garantie`)
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := [][]Term{
		{{Text: "facture"}},
		{{Text: "bail à loyer", Phrase: true}},
		{{Text: "assura"}, {Text: "css"}},
		{{Text: "décompte", Field: "title"}},
	}
	if !reflect.DeepEqual(q.Groups, wantGroups) {
		t.Errorf("groups %+v", q.Groups)
	}
	if !reflect.DeepEqual(q.Not, []Term{{Text: "brouillon"}}) {
		t.Errorf("not %+v", q.Not)
	}
	if len(q.Nears) != 1 || q.Nears[0].A.Text != "loyer" || q.Nears[0].B.Text != "garantie" || q.Nears[0].Distance != 5 {
		t.Errorf("nears %+v", q.Nears)
	}
	if !reflect.DeepEqual(q.Types, []string{"pdf"}) || !reflect.DeepEqual(q.Classes, []string{"4.2"}) ||
		!reflect.DeepEqual(q.Tags, []string{"eve"}) || !reflect.DeepEqual(q.NotTags, []string{"jeannette"}) {
		t.Errorf("filters %+v", q)
	}
	if q.After != "2024-03-01" || q.Before != "2025-01-01" {
		t.Errorf("dates %s %s", q.After, q.Before)
	}
	if !reflect.DeepEqual(q.Is, []string{"pending"}) || !reflect.DeepEqual(q.IsNot, []string{"sealed"}) {
		t.Errorf("states %v %v", q.Is, q.IsNot)
	}
	if !q.Strict() || q.Free() != "facture bail à loyer assura css décompte loyer garantie" {
		t.Errorf("strict %v free %q", q.Strict(), q.Free())
	}
}

func TestParsePlainAndEdges(t *testing.T) {
	q, _ := Parse("  décompte  assura ")
	if q.Strict() || len(q.Groups) != 2 {
		t.Errorf("plain words: %+v", q)
	}
	q, _ = Parse("year:2023 l'assurance")
	if q.After != "2023-01-01" || q.Before != "2024-01-01" || !q.Groups[0][0].Phrase || q.Groups[0][0].Text != "l assurance" {
		t.Errorf("year and elision: %+v", q)
	}
	q, _ = Parse(`title:"bail à loyer" http://example.org`)
	if q.Groups[0][0] != (Term{Text: "bail à loyer", Phrase: true, Field: "title"}) || q.Groups[1][0].Text != "http example org" {
		t.Errorf("quoted field and unknown field: %+v", q.Groups)
	}
	q, _ = Parse("type:pdf is:archived")
	if q.HasText() {
		t.Error("filters only must have no text")
	}
	for _, bad := range []string{`"open`, "OR facture", "facture OR", "is:shiny", "after:soon", "AROUND(3) x"} {
		var e *Error
		if _, err := Parse(bad); !errors.As(err, &e) || e.Hint == "" {
			t.Errorf("%q: want an error with a hint, got %v", bad, err)
		}
	}
}
