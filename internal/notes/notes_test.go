package notes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aclemen1/artefact-cli/internal/config"
)

func TestAddHandsTheActor(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "jot")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@|$JOT_BY|$OFFICE_ACTOR\" > "+out+"\n"), 0o755)
	cfg := &config.Config{}
	cfg.Notes.Add = []string{script, "add", "{ref}", "--by", "{by}"}
	if err := Add(cfg, "01X", "perso", "texte", "human:AC@tui"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if want := "add artefact:01X --by human:AC@tui|human:AC@tui|human:AC@tui\n"; string(b) != want {
		t.Errorf("got %q", b)
	}
}
