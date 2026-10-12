package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aclemen1/tuikit"
	"github.com/charmbracelet/x/ansi"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// The tests never write the real focus file, and the debounce does not slow them.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "artefact-tui-focus-")
	os.Setenv("OFFICE_FOCUS", filepath.Join(dir, "focus.json"))
	tuikit.FocusDelay = time.Millisecond
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fake replaces the external commands: each call is recorded.
type fake struct {
	calls  [][]string
	stdins []string
	answer func(argv []string) string
}

func (f *fake) install(t *testing.T) {
	old := runExternal
	runExternal = func(argv []string, stdin string) ([]byte, error) {
		f.calls = append(f.calls, argv)
		f.stdins = append(f.stdins, stdin)
		return []byte(f.answer(argv)), nil
	}
	t.Cleanup(func() { runExternal = old })
}

func withArtefacts(t *testing.T) (*model, store.ListItem) {
	t.Helper()
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso", "pro"}}
	st, err := store.Open(cfg, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Init()
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "pro", Filename: "b.pdf", Title: "Bail"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 140, 40
	drive(t, m, m.load())
	m.selectInList(a.URI)
	it := m.current()
	if it == nil || it.ID != a.ID {
		t.Fatalf("no selection: %+v", it)
	}
	return m, *it
}

func TestFocusWrittenAtSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.json")
	t.Setenv("OFFICE_FOCUS", path)
	m, _ := withArtefacts(t)
	press(t, m, "G")
	press(t, m, "g")
	press(t, m, "g")
	a := m.current()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]string
	json.Unmarshal(b, &f)
	if f["ref"] != "artefact:"+a.ID || f["title"] != a.Title || f["tool"] != "artefact" || f["sphere"] != a.Sphere || f["at"] == "" {
		t.Fatalf("focus: %s", b)
	}
	if m.syncFocus() != nil {
		t.Fatal("the same selection writes nothing more")
	}
}

func TestGreffierLine(t *testing.T) {
	m, a := withArtefacts(t)
	press(t, m, "i")
	if m.modal.Open() || !strings.Contains(m.status, "greffier.text") {
		t.Fatalf("i without greffier.text says so: %q", m.status)
	}
	m.cfg.Greffier.Text = []string{"greffier", "text", "-", "--focus", "{ref}", "--sphere", "{sphere}"}
	f := &fake{answer: func([]string) string {
		return `{"ok":true,"result":{"handled":false,"doubts":[{"question":"Which dossier?"}],"proposal":{"class":"task","sphere":"perso","dossier":"P-0024","dossier_title":"MAMAN","status":"doubt","writes":[{"key":"task"}]}}}`
	}}
	f.install(t)
	press(t, m, "i")
	if !m.modal.Open() {
		t.Fatal("i opens a free text")
	}
	typeText(t, m, "renew the insurance")
	press(t, m, "ctrl+s")
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != "greffier text - --focus artefact:"+a.ID+" --sphere perso" {
		t.Fatalf("argv: %q", f.calls)
	}
	if f.stdins[0] != "renew the insurance" {
		t.Fatalf("the text goes on stdin: %q", f.stdins[0])
	}
	if !strings.Contains(m.status, "greffier: task · perso · P-0024 MAMAN · task (doubt)") || !strings.Contains(m.status, "doubt: Which dossier?") {
		t.Fatalf("status: %q", m.status)
	}
}

func TestCabinetEmpty(t *testing.T) {
	m, _ := withArtefacts(t)
	press(t, m, "@")
	if !strings.Contains(m.status, "CABINET: no command configured") || m.modal.Open() {
		t.Fatalf("@ empty: %q", m.status)
	}
}

func TestUndo(t *testing.T) {
	m, a := withArtefacts(t)
	press(t, m, "u")
	if !strings.Contains(m.status, "no command configured") {
		t.Fatalf("u without undo says so: %q", m.status)
	}
	m.cfg.Undo.Preview = []string{"undo", "last", "--ref", "{ref}", "--dry-run"}
	m.cfg.Undo.Last = []string{"undo", "last", "--ref", "{ref}"}
	ref := "artefact:" + a.ID
	id := ref + "@2026-10-12T11:00:00+02:00"
	irreversible := false
	f := &fake{answer: func(argv []string) string {
		if strings.Contains(strings.Join(argv, " "), "--dry-run") {
			e := map[string]any{"entry": map[string]any{"id": id, "type": "archive", "text": "archived « Assura »", "by": "agent:greffier",
				"undo": []string{"artefact", "unarchive", a.ID}, "irreversible": irreversible}, "command": []string{"artefact", "unarchive", a.ID}, "dry_run": true}
			b, _ := json.Marshal(map[string]any{"ok": true, "result": e})
			return string(b)
		}
		return `{"ok":false,"error":{"code":6,"kind":"needs_validation","message":"cannot be undone in turn"}}`
	}}
	f.install(t)
	press(t, m, "u")
	if !m.modal.Open() {
		t.Fatalf("u asks to confirm: %q", m.status)
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"archived « Assura »", "artefact unarchive " + a.ID, "agent:greffier"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the confirmation shows %q:\n%s", want, view)
		}
	}
	if got := strings.Join(f.calls[0], " "); got != "undo last --ref "+ref+" --dry-run --format json" {
		t.Fatalf("preview: %s", got)
	}
	press(t, m, "y")
	if m.modal.Open() {
		press(t, m, "enter")
	}
	if len(f.calls) != 2 || strings.Join(f.calls[1], " ") != "undo last --ref "+ref+" --format json" {
		t.Fatalf("last: %q (%q)", f.calls, m.status)
	}
	want := "cannot be undone from here: validate it in undo tui --select " + id
	if m.status != want {
		t.Fatalf("needs_validation: %q", m.status)
	}

	// An irreversible entry is not even proposed.
	irreversible = true
	f.calls = nil
	press(t, m, "u")
	if m.modal.Open() || m.status != want || len(f.calls) != 1 {
		t.Fatalf("irreversible: open=%v %q %q", m.modal.Open(), m.status, f.calls)
	}
}

// The journal marks the entries that carry an undo, only when undo.last is configured.
func TestJournalUndoable(t *testing.T) {
	m, a := withArtefacts(t)
	m.cfg.History.Ls = []string{"logbook", "ls", "--ref", "{ref}", "--limit", "{limit}", "--format", "json"}
	f := &fake{answer: func([]string) string {
		return `{"ok":true,"result":{"items":[{"id":"x@1","at":"2026-10-12T11:00:00+02:00","type":"archive","text":"archived","by":"agent:greffier","undo":["artefact","unarchive","X"]}],"count":1}}`
	}}
	f.install(t)
	press(t, m, "enter")
	if got := strings.Join(f.calls[0], " "); got != "logbook ls --ref artefact:"+a.ID+" --limit 10 --format json" {
		t.Fatalf("history.ls: %s", got)
	}
	s := screen(m)
	if !strings.Contains(s, "Journal (1)") || strings.Contains(s, "↶ undoable") {
		t.Fatalf("journal without mark:\n%s", s)
	}
	m.cfg.Undo.Last = []string{"undo", "last", "--ref", "{ref}"}
	if !strings.Contains(screen(m), "↶ undoable") {
		t.Fatalf("mark missing:\n%s", screen(m))
	}
}

// expand fills the variables of a command of the configuration.
func TestExpand(t *testing.T) {
	got := expand([]string{"x", "{ref}", "{id}", "{sphere}", "{limit}"}, "artefact:01A", "pro", 7)
	if strings.Join(got, " ") != "x artefact:01A 01A pro 7" {
		t.Fatalf("expand: %q", got)
	}
}

// A command launched by the TUI signs human:AC@tui, whatever the shell left.
func TestCommandsCarryTheTUIActor(t *testing.T) {
	t.Setenv("OFFICE_ACTOR", "agent:office-P-0019")
	t.Setenv("GREFFIER_BY", "agent:greffier")
	script := filepath.Join(t.TempDir(), "greffier")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$GREFFIER_BY $OFFICE_ACTOR\"\n"), 0o755)
	out, err := runExternal([]string{script}, "")
	if err != nil || strings.TrimSpace(string(out)) != "human:AC@tui human:AC@tui" {
		t.Fatalf("env: %q %v", out, err)
	}
}
