package tui

import (
	"os"
	"path/filepath"
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

func TestOpenSealedAsksThenCleansUp(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}, DefaultSphere: "perso"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	id, rcp, _ := keys.Generate()
	cfg.Keys = map[string]string{"perso": rcp}
	st.Keys = keys.Memory{"perso": id}
	a, _ := st.Put(strings.NewReader("TOPSECRET"), store.PutOptions{Filename: "codes.txt", Seal: true})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "o")
	if m.prompt != promptReveal {
		t.Fatalf("no confirmation asked: %q", m.status)
	}
	typeText(t, m, "no")
	press(t, m, "enter")
	if len(m.revealed) != 0 {
		t.Fatal("decrypted without yes")
	}
	press(t, m, "o")
	typeText(t, m, "yes")
	press(t, m, "enter")
	if len(m.revealed) != 1 {
		t.Fatalf("not decrypted: %q", m.status)
	}
	p := m.revealed[0] + "/codes.txt"
	b, err := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if err != nil || string(b) != "TOPSECRET" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("decrypted copy: %q %v %v", b, err, fi.Mode())
	}
	m.Cleanup()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("decrypted copy left after cleanup")
	}
	_ = a
}

func TestEditContent(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}, DefaultSphere: "perso"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("draft"), store.PutOptions{Filename: "note.md"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "enter")
	press(t, m, "E")
	e := m.edits[a.ID]
	if e == nil {
		t.Fatalf("no working copy: %q", m.status)
	}
	os.WriteFile(e.path, []byte("final"), 0o600)
	_, cmd := m.Update(editedContentMsg{id: a.ID})
	drive(t, m, cmd)
	if m.prompt != promptRevise {
		t.Fatalf("no save asked: %q", m.status)
	}
	typeText(t, m, "yes")
	press(t, m, "enter")
	rc, _, _ := st.OpenPayload(a.ID, "", false)
	b := make([]byte, 5)
	rc.Read(b)
	rc.Close()
	if string(b) != "final" {
		t.Errorf("content %q, status %q", b, m.status)
	}
	stat, _, _ := st.Stat([]string{a.ID}, false)
	if len(stat[0].Versions) != 2 {
		t.Errorf("versions %d", len(stat[0].Versions))
	}
	os.RemoveAll(filepath.Dir(e.path))
}

func TestPlansView(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}, DefaultSphere: "perso"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n  - notation: \"4\"\n    label: Housing\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Filename: "b.pdf", Title: "Bail"})
	st.SetClass(a.ID, "h", "3.2", "alain")
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "C")
	if m.planView == nil || len(m.planView.rows) != 4 {
		t.Fatalf("plan rows, classes closed: %+v", m.planView)
	}
	press(t, m, "j") // 3 Health
	press(t, m, "l") // opens it: 3.2 shows
	press(t, m, "j") // 3.2 Insurance
	press(t, m, "l") // opens it: the artefact Assura shows
	rows := m.planView.rows
	if len(rows) != 6 || rows[3].item == nil || rows[3].item.ID != a.ID || rows[3].branch != "│       └── " {
		t.Fatalf("open tree: %+v", rows)
	}
	counts := map[string]int{}
	for _, r := range rows {
		if r.item == nil {
			counts[r.notation] = r.count
		}
	}
	if counts["3"] != 1 || counts["3.2"] != 1 || counts["4"] != 0 || counts[unclassifiedRow] != 1 {
		t.Errorf("counts %v", counts)
	}
	press(t, m, "t") // deposited -> date: the tree keeps its rows
	if len(m.planView.rows) != 6 || m.order.Key != "date" {
		t.Fatalf("t in the tree: %s, %d rows", m.order, len(m.planView.rows))
	}
	press(t, m, "j")
	press(t, m, "enter") // the artefact's detail
	if m.mode != modeDetail {
		t.Fatal("enter on an artefact must show its detail")
	}
	press(t, m, "esc") // back to the tree
	if m.planView == nil || m.mode == modeDetail {
		t.Fatal("esc from the detail must return to the tree")
	}
	press(t, m, "h") // up to 3.2
	press(t, m, "h") // closes 3.2
	press(t, m, "h") // up to 3
	if r := m.planView.rows[m.planView.sel]; r.notation != "3" || len(m.planView.rows) != 5 {
		t.Fatalf("after h: on %q, %d rows", r.notation, len(m.planView.rows))
	}
	press(t, m, "enter")
	if l := m.visible(); len(l) != 1 || l[0].ID != a.ID {
		t.Fatalf("class 3 list: %v", l)
	}
	press(t, m, "C") // clears the class filter
	if len(m.visible()) != 2 {
		t.Fatalf("after clearing: %d", len(m.visible()))
	}
	for i, it := range m.visible() {
		if it.Title == "Bail" {
			m.sel = i
		}
	}
	press(t, m, "c")
	for i, r := range m.planView.rows {
		if r.notation == "4" {
			m.planView.sel = i
		}
	}
	press(t, m, "enter")
	if l, _ := st.List("perso", indexFilterClass("h:4")); len(l) != 1 {
		t.Errorf("Bail not placed in 4: %v (%q)", l, m.status)
	}
	press(t, m, "e")
	if m.editCard == nil {
		t.Error("e from the list must open the card editor")
	}
}
