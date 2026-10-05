package store

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/meta"
)

func TestReviseKeepsVersions(t *testing.T) {
	s := sealedStore(t)
	r := put(t, s, "first", PutOptions{Filename: "n.md"})
	p := filepath.Join(t.TempDir(), "n.md")
	os.WriteFile(p, []byte("second"), 0o644)
	res, err := s.Revise(r.ID, p)
	if err != nil || res.Version != 2 || res.VersionsErased != 0 {
		t.Fatalf("revise %+v %v", res, err)
	}
	if read(t, s, r.ID, "v1", false) != "first" || read(t, s, r.ID, "", false) != "second" {
		t.Error("versions after revise")
	}
	st, _, _ := s.Stat([]string{r.ID}, true)
	if ev := st[0].Events[len(st[0].Events)-1]; ev.Type != "revise" {
		t.Errorf("last event %+v", ev)
	}
	sealed := put(t, s, "secret one", PutOptions{Filename: "s.txt", Seal: true})
	os.WriteFile(p, []byte("secret two"), 0o644)
	if _, err := s.Revise(sealed.ID, p); err != nil {
		t.Fatal(err)
	}
	if read(t, s, sealed.ID, "", true) != "secret two" || grepStore(t, s, "perso", "secret two") {
		t.Error("a revised sealed artefact must stay sealed")
	}
}

func TestTags(t *testing.T) {
	s := newStore(t)
	a := put(t, s, "x", PutOptions{Filename: "a.txt"})
	put(t, s, "y", PutOptions{Filename: "b.txt"})
	if _, err := s.Tag(a.ID, []string{"avs", "jeannette", "avs"}, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List("", index.Filter{Tag: "avs"}); len(l) != 1 || l[0].ID != a.ID {
		t.Errorf("ls --tag: %v", l)
	}
	if h, _ := s.Search("jeannette", "", 5, "all"); len(h) != 1 {
		t.Errorf("search by tag: %v", h)
	}
	c, _ := s.Tag(a.ID, nil, []string{"avs"})
	if len(c.Tags) != 1 || c.Tags[0] != "jeannette" {
		t.Errorf("untag: %v", c.Tags)
	}
	if n, _ := s.TagCounts(""); n["jeannette"] != 1 || n["avs"] != 0 {
		t.Errorf("counts %v", n)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	if !meta.Available() {
		t.Skip("exiftool not installed")
	}
	s := newStore(t)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 3)))
	r := put(t, s, buf.String(), PutOptions{Filename: "photo.png"})
	if _, err := s.EditCard(r.ID, []string{"title=Plage de La Tour", "date=2024-07-23", "dc.creator=Alain Clément"}, ""); err != nil {
		t.Fatal(err)
	}
	s.Tag(r.ID, []string{"vacances", "léman"}, nil)
	w, err := s.WriteMeta(r.ID)
	if err != nil || w.Version != 2 {
		t.Fatalf("write %+v %v", w, err)
	}
	m, err := s.ReadMeta(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := m.Embedded
	if e == nil || e.Title != "Plage de La Tour" || e.Date != "2024-07-23" || e.Author != "Alain Clément" || strings.Join(e.Keywords, ",") != "vacances,léman" || e.Width != 4 {
		t.Errorf("embedded %+v", e)
	}
	// A deposit takes the content's own date.
	p := filepath.Join(t.TempDir(), "x.png")
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	os.WriteFile(p, buf.Bytes(), 0o644)
	meta.Write(p, meta.Fields{Date: "2019-05-02"})
	b, _ := os.ReadFile(p)
	r2 := put(t, s, string(b), PutOptions{Filename: "x.png"})
	st, _, _ := s.Stat([]string{r2.ID}, false)
	if st[0].Card.Date != "2019-05-02" {
		t.Errorf("deposit date from the file: %s", st[0].Card.Date)
	}
	if _, err := s.WriteMeta(put(t, s, "plain", PutOptions{Filename: "n.md"}).ID); err == nil {
		t.Error("metadata cannot be written into a Markdown file")
	}
}
