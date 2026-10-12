package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartRunsTheCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "j.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n{ echo \"$@\"; cat; } > "+out+".tmp && mv "+out+".tmp "+out+"\n"), 0o755)
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	cmd := []string{script, "add", "-", "--source", "artefact:{id}", "--type", "{type}", "--sphere", "{sphere}", "--by", "{by}", "--at", "{at}"}
	if err := Start(cmd, Entry{ID: "cli", Type: "unsigned", Sphere: "perso", By: "unknown:cli", At: at, Text: "outil : artefact\n"}); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for i := 0; i < 100; i++ {
		if b, _ = os.ReadFile(out); len(b) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := "add - --source artefact:cli --type unsigned --sphere perso --by unknown:cli --at 2026-10-15T09:00:00Z\noutil : artefact\n"
	if string(b) != want {
		t.Errorf("got %q", b)
	}
	if err := Start(nil, Entry{}); err != nil {
		t.Error(err)
	}
	if !strings.Contains(strings.Join(Argv([]string{"{id}"}, Entry{ID: "x"}), ""), "x") {
		t.Error("argv")
	}
}
