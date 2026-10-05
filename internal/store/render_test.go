package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLector answers like lector read: the payload's bytes upper-cased, as Markdown.
func fakeLector(t *testing.T, s *Store) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls")
	script := filepath.Join(t.TempDir(), "lector")
	body := `#!/bin/sh
echo "$2" >> ` + log + `
if [ "$2" = - ]; then text=$(tr a-z A-Z); else text=$(tr a-z A-Z < "$2"); fi
printf '{"ok":true,"result":{"markdown":"%s","methods":{"1":"text"},"confidence":1,"range":{"truncated":false}}}' "$text"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Cfg.Lector.Command = []string{script}
	return log
}

func TestRenderFeedsSearchAndGoesAwayOnSeal(t *testing.T) {
	s := sealedStore(t)
	calls := fakeLector(t, s)
	r := put(t, s, "quittance loyer octobre", PutOptions{Filename: "scan.pdf", Mime: "application/pdf"})

	res, err := s.Render(r.URI, false)
	if err != nil || res.Status != "rendered" || res.Rendition.Chars == 0 {
		t.Fatalf("render: %+v %v", res, err)
	}
	if res, _ = s.Render(r.URI, false); res.Status != "current" {
		t.Fatalf("second render = %s, want current", res.Status)
	}
	text, _, err := s.RenditionText(r.URI)
	if err != nil || text != "QUITTANCE LOYER OCTOBRE" {
		t.Fatalf("text = %q %v", text, err)
	}
	hits, err := s.Search("quittance", "", 10, "")
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %v %v", hits, err)
	}
	if left, _ := s.Unrendered("perso"); len(left) != 0 {
		t.Fatalf("unrendered = %v", left)
	}

	if _, err := s.Seal(r.URI); err != nil {
		t.Fatal(err)
	}
	ref, _ := s.Resolve(r.URI)
	if _, err := os.Stat(filepath.Join(ref.Dir, extDir, "renditions")); !os.IsNotExist(err) {
		t.Fatalf("renditions survive the seal: %v", err)
	}
	if hits, _ := s.Search("quittance", "", 10, ""); len(hits) != 0 {
		t.Fatalf("sealed text still searchable: %v", hits)
	}
	before, _ := os.ReadFile(calls)
	if res, _ := s.Render(r.URI, true); res.Status != "sealed" {
		t.Fatalf("render of sealed = %s", res.Status)
	}
	after, _ := os.ReadFile(calls)
	if len(after) != len(before) {
		t.Fatal("lector was called on a sealed artefact")
	}
	if left, _ := s.Unrendered("perso"); len(left) != 0 {
		t.Fatalf("sealed listed as unrendered: %v", left)
	}
}

func TestReviseDropsRendition(t *testing.T) {
	s := newStore(t)
	fakeLector(t, s)
	r := put(t, s, "first", PutOptions{Filename: "a.pdf", Mime: "application/pdf"})
	if _, err := s.Render(r.URI, false); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "b.pdf")
	os.WriteFile(f, []byte("second"), 0o644)
	if _, err := s.Revise(r.URI, f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RenditionText(r.URI); err == nil || !strings.Contains(err.Error(), "no text rendition") {
		t.Fatalf("stale rendition served: %v", err)
	}
	s.Render(r.URI, false)
	if text, _, _ := s.RenditionText(r.URI); text != "SECOND" {
		t.Fatalf("text = %q", text)
	}
}
