package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// drive runs a command and feeds its messages back, batches included.
func drive(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drive(t, m, c)
		}
	case nil, tickMsg:
	default:
		if _, isBG := msg.(tea.BackgroundColorMsg); isBG {
			return
		}
		_, next := m.Update(msg)
		drive(t, m, next)
	}
}

func press(t *testing.T, m *model, s string) {
	t.Helper()
	var k tea.KeyPressMsg
	switch s {
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		r := []rune(s)[0]
		k = tea.KeyPressMsg{Code: r, Text: s}
	}
	_, cmd := m.Update(k)
	drive(t, m, cmd)
}

func typeText(t *testing.T, m *model, s string) {
	for _, r := range s {
		press(t, m, string(r))
	}
}

func screen(m *model) string { return ansi.Strip(m.render()) }

func TestTUI(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso", "pro"}, DefaultSphere: "perso"}
	st, err := store.Open(cfg, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	id, rcp, _ := keys.Generate()
	cfg.Keys = map[string]string{"perso": rcp}
	st.Keys = keys.Memory{"perso": id}
	a, _ := st.Put(strings.NewReader("ordonnance"), store.PutOptions{Filename: "ordonnance.pdf", Title: "Ordonnance septembre"})
	st.Put(strings.NewReader("facture"), store.PutOptions{Filename: "facture.pdf", Title: "Facture Romande Energie", Sphere: "pro", Holds: []index.Holder{{By: "mnemo", Ref: "C"}}})

	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 140, 40
	drive(t, m, m.Init())
	s := screen(m)
	if !strings.Contains(s, "Ordonnance septembre") || !strings.Contains(s, "Facture Romande Energie") {
		t.Fatalf("list:\n%s", s)
	}

	press(t, m, "s") // perso only
	if s := screen(m); strings.Contains(s, "Facture") || !strings.Contains(s, "Ordonnance") {
		t.Fatalf("perso filter:\n%s", s)
	}
	press(t, m, "s") // pro
	press(t, m, "s") // all again
	press(t, m, "/")
	typeText(t, m, "factu")
	press(t, m, "enter")
	if s := screen(m); strings.Contains(s, "Ordonnance") || !strings.Contains(s, "Facture") {
		t.Fatalf("text filter:\n%s", s)
	}
	press(t, m, "r") // back to the whole list

	// Detail of the perso artefact, then seal it.
	for i, it := range m.visible() {
		if it.ID == a.ID {
			m.sel = i
		}
	}
	press(t, m, "enter")
	if s := screen(m); !strings.Contains(s, "Versions (1)") || !strings.Contains(s, "orphaned") {
		t.Fatalf("detail:\n%s", s)
	}
	press(t, m, "S")
	typeText(t, m, "yes")
	press(t, m, "enter")
	if s := screen(m); !strings.Contains(s, "sealed") {
		t.Fatalf("after seal:\n%s", s)
	}

	// Erase: the plan is shown, a wrong code cancels, the right one erases.
	press(t, m, "x")
	if m.plan == nil || !strings.Contains(screen(m), "Erasure plan") {
		t.Fatalf("no plan: %s", m.status)
	}
	typeText(t, m, "NOPE-NOPE")
	press(t, m, "enter")
	if _, err := st.Resolve(a.ID); err != nil {
		t.Fatal("a wrong code erased the artefact")
	}
	press(t, m, "x")
	typeText(t, m, m.plan.Code)
	press(t, m, "enter")
	if _, err := st.Resolve(a.ID); err == nil {
		t.Fatalf("not erased: %s", m.status)
	}
	if m.mode != modeList || !strings.Contains(m.status, "erased") {
		t.Errorf("after erase: mode %v status %q", m.mode, m.status)
	}

	// A held artefact needs X.
	m.sel = 0
	press(t, m, "enter")
	press(t, m, "x")
	if m.plan != nil || !m.statusErr {
		t.Errorf("x on a held artefact must refuse: %q", m.status)
	}
}
