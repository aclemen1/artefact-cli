package tui

import (
	"github.com/aclemen1/tuikit"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	case "tab":
		k = tea.KeyPressMsg{Code: tea.KeyTab}
	case "right":
		k = tea.KeyPressMsg{Code: tea.KeyRight}
	case "ctrl+s":
		k = tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
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
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso", "pro"}}
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
	a, _ := st.Put(strings.NewReader("ordonnance"), store.PutOptions{Sphere: "perso", Filename: "ordonnance.pdf", Title: "Ordonnance septembre"})
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
	press(t, m, "#")
	if m.plan == nil || !strings.Contains(screen(m), "Erasure plan") {
		t.Fatalf("no plan: %s", m.status)
	}
	typeText(t, m, "NOPE-NOPE")
	press(t, m, "enter")
	if _, err := st.Resolve(a.ID); err != nil {
		t.Fatal("a wrong code erased the artefact")
	}
	press(t, m, "esc") // cancels; the code is left unused
	if m.modal.Open() {
		t.Fatal("esc must close the erasure")
	}
	press(t, m, "#")
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
	press(t, m, "#")
	if m.plan != nil || !m.statusErr {
		t.Errorf("x on a held artefact must refuse: %q", m.status)
	}
}

func TestOpenSealedAsksThenCleansUp(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	id, rcp, _ := keys.Generate()
	cfg.Keys = map[string]string{"perso": rcp}
	st.Keys = keys.Memory{"perso": id}
	a, _ := st.Put(strings.NewReader("TOPSECRET"), store.PutOptions{Sphere: "perso", Filename: "codes.txt", Seal: true})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "o")
	if !m.modal.Open() {
		t.Fatalf("no confirmation asked: %q", m.status)
	}
	typeText(t, m, "n") // Non ("o" is Oui)
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
	if err != nil || string(b) != "TOPSECRET" || fi.Mode().Perm() != 0o400 {
		t.Fatalf("decrypted copy: %q %v %v", b, err, fi.Mode())
	}
	// A second opening replaces the read-only copy left by the first.
	if _, err := m.decrypt(a.URI); err != nil {
		t.Fatalf("second opening: %v", err)
	}
	m.Cleanup()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("decrypted copy left after cleanup")
	}
	_ = a
}

func TestEditContent(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("draft"), store.PutOptions{Sphere: "perso", Filename: "note.md"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "enter")
	press(t, m, "M")
	e := m.edits[a.ID]
	if e == nil {
		t.Fatalf("no working copy: %q", m.status)
	}
	os.WriteFile(e.path, []byte("final"), 0o600)
	_, cmd := m.Update(editedContentMsg{id: a.ID})
	drive(t, m, cmd)
	if !m.modal.Open() {
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
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n  - notation: \"4\"\n    label: Housing\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.SetClass(a.ID, "h", "3.2", "alain")
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	m.sel = -1 // nothing selected: the tree opens with its classes closed
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
	press(t, m, "m")
	for i, r := range m.planView.rows {
		if r.notation == "4" {
			m.planView.sel = i
		}
	}
	press(t, m, "enter")
	if l, _ := st.List("perso", indexFilterClass("h:4")); len(l) != 1 {
		t.Errorf("Bail not placed in 4: %v (%q)", l, m.status)
	}
	press(t, m, "E")
	if m.editCard == nil {
		t.Error("E from the list must open the card editor")
	}
}

func TestTreeDoesWhatTheListDoes(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n  - notation: \"4\"\n    label: Housing\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.SetClass(a.ID, "h", "3", "alain")
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	m.sel = -1 // nothing selected: the tree opens with its classes closed
	press(t, m, "C")

	press(t, m, "/")
	typeText(t, m, "assu")
	rows := m.planView.rows // Household, 3 (opened by the match), Assura
	if len(rows) != 3 || rows[2].item == nil || rows[2].item.ID != a.ID {
		t.Fatalf("filtered tree: %+v", rows)
	}
	press(t, m, "enter") // full-text search, still in the tree
	if m.planView == nil || !m.searched || len(m.planView.rows) != 3 {
		t.Fatalf("search in the tree: %+v", m.planView)
	}
	m.planView.sel = 2
	press(t, m, "E")
	if m.editCard == nil || m.editCard.ID != a.ID {
		t.Fatal("E in the tree must edit the selected artefact's card")
	}
	press(t, m, "esc") // closes the card form, unchanged
	if m.modal.Open() || m.editCard != nil {
		t.Fatal("esc must close an unchanged card form")
	}
	press(t, m, "esc") // clears the filter, stays in the tree
	if m.planView == nil || m.filter != "" || len(m.planView.rows) != 4 {
		t.Fatalf("after esc: %+v", m.planView)
	}

	press(t, m, "j")
	press(t, m, "l") // opens 3 Health
	press(t, m, "j") // Assura
	press(t, m, "m") // place it elsewhere
	if m.planView == nil || m.planView.assign == nil {
		t.Fatal("m in the tree must open the class picker")
	}
	for i, r := range m.planView.rows {
		if r.notation == "4" {
			m.planView.sel = i
		}
	}
	press(t, m, "enter")
	if m.planView == nil || m.planView.assign != nil {
		t.Fatalf("after placing, the browsing tree comes back: %+v", m.planView)
	}
	if l, _ := st.List("perso", indexFilterClass("h:4")); len(l) != 1 {
		t.Errorf("Assura not placed in 4: %v (%q)", l, m.status)
	}
}

func TestWatchReloadsOnOutsideChangesOnly(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "A"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	tickOnce := func() tea.Cmd {
		var cmds []tea.Cmd
		cmds = append(cmds, m.refreshCopies(), m.checkEdits())
		drive(t, m, tea.Batch(cmds...))
		return m.onVersions(m.watchStore()().(versionsMsg))
	}
	tickOnce()
	for i := range 3 {
		if cmd := tickOnce(); cmd != nil {
			t.Fatalf("idle tick %d reloads", i)
		}
	}
	// Another process deposits.
	other, _ := store.Open(cfg, nil, "other")
	other.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "B"})
	other.Close()
	cmd := tickOnce()
	if cmd == nil {
		t.Fatal("an outside deposit must reload the list")
	}
	drive(t, m, cmd)
	if len(m.visible()) != 2 {
		t.Fatalf("after reload: %d artefacts", len(m.visible()))
	}
	if cmd := tickOnce(); cmd != nil {
		t.Fatal("the reload itself must not trigger another one")
	}
	// Another process edits a card.
	other, _ = store.Open(cfg, nil, "other")
	time.Sleep(1100 * time.Millisecond) // card times have a one-second grain
	other.EditCard(m.visible()[0].URI, []string{"title=Renamed"}, "")
	other.Close()
	if cmd := tickOnce(); cmd == nil {
		t.Fatal("an outside card edit must reload the list")
	}
}

func TestSearchShowsItsPhase(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Put(strings.NewReader("quittance de loyer"), store.PutOptions{Sphere: "perso", Filename: "q.txt", Title: "Quittance"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "/")
	typeText(t, m, "quittance")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.searching != "words" || !strings.Contains(m.footer(), "searching by words") {
		t.Fatalf("while searching: %q / %q", m.searching, m.footer())
	}
	drive(t, m, cmd)
	if m.searching != "" || len(m.visible()) != 1 || !strings.Contains(m.status, "1 found by words") {
		t.Fatalf("after: searching %q, %d hits, status %q", m.searching, len(m.visible()), m.status)
	}
}

func TestEscGoesBackQQuits(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n"), false)
	st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	fresh := func() *model {
		m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
		m.w, m.h = 120, 30
		drive(t, m, m.load()) // Init would wait for a 3 s tick
		return m
	}
	quits := func(m *model, key string) bool {
		var k tea.KeyPressMsg
		if key == "esc" {
			k = tea.KeyPressMsg{Code: tea.KeyEscape}
		} else {
			k = tea.KeyPressMsg{Code: []rune(key)[0], Text: key}
		}
		_, cmd := m.Update(k)
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg) // a batch is not run: q answers tea.Quit itself
		return ok
	}
	views := map[string]func(*model){
		"list":   func(m *model) {},
		"detail": func(m *model) { press(t, m, "enter") },
		"plans":  func(m *model) { press(t, m, "C") },
		"assign": func(m *model) { press(t, m, "m") },
		"keys":   func(m *model) { press(t, m, "?") },
	}
	for name, open := range views {
		m := fresh()
		open(m)
		if quits(m, "esc") {
			t.Errorf("esc quits from %s", name)
		}
		m = fresh()
		open(m)
		if !quits(m, "q") {
			t.Errorf("q does not quit from %s", name)
		}
	}
	m := fresh()
	press(t, m, "/")
	if quits(m, "q") || !strings.Contains(m.input.Value(), "q") {
		t.Error("in a field, q must be typed")
	}
	m = fresh()
	press(t, m, "enter")
	press(t, m, "esc")
	if m.mode != modeList {
		t.Error("esc from the detail must come back to the list")
	}
}

func TestReadOnlyEditor(t *testing.T) {
	for in, want := range map[string]string{
		"nvim":                  "nvim -M",
		"/opt/homebrew/bin/vim": "/opt/homebrew/bin/vim -M",
		"nano":                  "nano -v",
		"code --wait":           "code --wait",
	} {
		if got := readOnlyEditor(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestViewPathPrefersAMountedView(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	it := store.ListItem{Row: index.Row{ID: "01M465PV5GC0HRTDEXRKE18C81", Ext: ".pdf"}, Sphere: "zz-test-sphere"}
	cfg := &config.Config{}
	cfg.Views.Mounts = map[string]config.Dirs{"zz-test-sphere": {dir}}
	if p := viewPath(cfg, nil, it, ""); p != "" {
		t.Fatalf("no file yet, got %s", p)
	}
	os.WriteFile(filepath.Join(dir, "a", it.ID+".pdf"), []byte("x"), 0o644)
	if p := viewPath(cfg, nil, it, ""); p != filepath.Join(dir, "a", it.ID+".pdf") {
		t.Fatalf("view path %q", p)
	}
}

func TestSelectionFollowsBetweenListAndPlans(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n  - notation: \"4\"\n    label: Housing\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.Put(strings.NewReader("c"), store.PutOptions{Sphere: "perso", Filename: "c.pdf", Title: "Chauffage"})
	st.SetClass(a.ID, "h", "3.2", "alain")
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.load())
	m.selectInList(a.URI)
	press(t, m, "C")
	if it := m.treeItem(); it == nil || it.ID != a.ID {
		t.Fatalf("the tree must open on Assura, under 3 › 3.2: %+v", m.planView.rows)
	}
	press(t, m, "j") // next row
	press(t, m, "k") // back on Assura
	press(t, m, "esc")
	if it := m.current(); m.planView != nil || it == nil || it.ID != a.ID {
		t.Fatalf("the list must come back on Assura: %+v", it)
	}
}

func TestReloadOnNewBinary(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.SetClass(a.ID, "h", "3", "alain")
	state := filepath.Join(t.TempDir(), "resume.json")
	resumePath = func() string { return state }
	defer func() { resumePath = func() string { return config.Expand("~/.local/state/artefact/tui-resume.json") } }()
	exe := filepath.Join(t.TempDir(), "artefact")
	os.WriteFile(exe, []byte("v1"), 0o755)
	locate := func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil }

	m := newModel(cfg, st, locate)
	m.w, m.h = 120, 30
	m.exe = exe
	m.exeStamp, _ = stampOf(exe)
	m.build = "0.1.0-dev test"
	drive(t, m, m.load())
	m.selectInList(a.URI)
	m.orphaned = false

	// A rebuild while the user types: a badge, no reload.
	press(t, m, "/")
	os.Remove(exe)
	os.WriteFile(exe, []byte("v2"), 0o755) // new inode, as go install does
	if cmd := m.checkBinary(); cmd != nil || m.reexec {
		t.Fatal("reload while typing")
	}
	if !strings.Contains(m.header(), "new version") {
		t.Fatalf("no badge: %s", m.header())
	}
	press(t, m, "esc")
	// At rest, the next check reloads, keeping the state.
	cmd := m.checkBinary()
	if cmd == nil || !m.reexec {
		t.Fatal("no reload at rest")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("the reload must quit to exec the new binary")
	}

	// The new process picks the state up.
	t.Setenv(resumeEnv, state)
	n := newModel(cfg, st, locate)
	n.w, n.h = 120, 30
	n.build = "0.1.0-dev test2"
	n.loadResume()
	if !strings.Contains(n.status, "reloaded") {
		t.Fatalf("status %q", n.status)
	}
	drive(t, n, n.load())
	if it := n.current(); it == nil || it.ID != a.ID {
		t.Fatalf("selection not carried over: %+v", it)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the resume file must be removed once read")
	}
}

func TestConventionKeys(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso", "pro"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.load())

	press(t, m, "G")
	press(t, m, "g")
	if m.sel == 0 {
		t.Fatal("a single g must wait for the second")
	}
	press(t, m, "g")
	if m.sel != 0 {
		t.Fatal("gg must go to the top")
	}

	m.selectInList(a.URI)
	press(t, m, "x")
	if l, _ := st.List("perso", index.Filter{State: "archived"}); len(l) != 1 || l[0].ID != a.ID {
		t.Fatalf("x must archive Assura: %v (%q)", l, m.status)
	}
	press(t, m, "4") // archives
	if !m.archived || len(m.visible()) != 1 {
		t.Fatalf("view 4: archived %v, %d rows", m.archived, len(m.visible()))
	}
	press(t, m, "e") // active again
	press(t, m, "1")
	if m.archived || len(m.visible()) != 2 {
		t.Fatalf("view 1: %d rows", len(m.visible()))
	}
	press(t, m, "2")
	if m.planView == nil {
		t.Fatal("view 2 must show the plans")
	}
	press(t, m, "1")

	f := filepath.Join(t.TempDir(), "scan.pdf")
	os.WriteFile(f, []byte("%PDF new"), 0o644)
	press(t, m, "c")
	if !m.modal.Open() {
		t.Fatal("c must open the deposit form")
	}
	typeText(t, m, f)
	press(t, m, "ctrl+s")
	if !m.modal.Open() {
		t.Fatal("the sphere is required: the form must stay open")
	}
	// ctrl+s put the cursor on the missing sphere.
	press(t, m, "right") // perso
	press(t, m, "right") // pro
	press(t, m, "ctrl+s")
	if l, _ := st.List("pro", index.Filter{}); len(l) != 1 || l[0].Title != "scan" {
		t.Fatalf("deposit: %v (%q) %s", l, m.status, tuikit.Overlay("", m.modal, 120, 30))
	}

	m.selectInList(a.URI)
	press(t, m, "#")
	if !m.modal.Open() || m.plan == nil {
		t.Fatalf("# must show the erasure plan: %q", m.status)
	}
	press(t, m, "esc")
}

func TestCardForm(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura", Date: "2026-09-03"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.load())
	m.selectInList(a.URI)
	press(t, m, "E")
	if !m.modal.Open() {
		t.Fatal("E must open the card form")
	}
	typeText(t, m, " 2026")
	press(t, m, "ctrl+s")
	st2, _, _ := st.Stat([]string{a.URI}, false)
	if c := st2[0].Card; c.Title != "Assura 2026" || c.Date != "2026-09-03" {
		t.Fatalf("card: title %q date %q (%q)", c.Title, c.Date, m.status)
	}
	if m.modal.Open() || !strings.Contains(m.status, "card saved") {
		t.Fatalf("after save: open %v status %q", m.modal.Open(), m.status)
	}
}

func TestCardFormTags(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	b, _ := st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.Tag(b.URI, []string{"eve"}, nil)
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 30
	drive(t, m, m.load())
	if got := m.tagCompleter("perso")("ev"); len(got) != 1 || got[0].Value != "eve" {
		t.Fatalf("completion: %+v", got)
	}
	m.selectInList(a.URI)
	press(t, m, "E")
	for range 4 { // title, date, file name, type
		press(t, m, "tab")
	}
	typeText(t, m, "eve,")
	typeText(t, m, "jeannette,")
	press(t, m, "ctrl+s")
	st2, _, _ := st.Stat([]string{a.URI}, false)
	if got := strings.Join(st2[0].Card.Tags, ","); got != "eve,jeannette" {
		t.Fatalf("tags %q (%q)", got, m.status)
	}
}

func TestNotes(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	dir := t.TempDir()
	script := filepath.Join(dir, "fakenote")
	os.WriteFile(script, []byte(`#!/bin/sh
d=`+dir+`
case "$1" in
add) echo "$2|$3" > "$d/args"; cat > "$d/body" ;;
ls) if [ -f "$d/body" ]; then printf '{"ok":true,"result":{"items":[{"id":"PN-0009","created":"2026-10-10","body":"%s"}]}}' "$(cat "$d/body")"; else printf '{"ok":true,"result":{"items":[]}}'; fi ;;
esac
`), 0o755)
	cfg.Notes.Add = []string{script, "add", "{ref}", "{sphere}"}
	cfg.Notes.Ls = []string{script, "ls", "{ref}"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	m := newModel(cfg, st, func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil })
	m.w, m.h = 120, 40
	drive(t, m, m.load())
	m.selectInList(a.URI)
	press(t, m, "N")
	if !m.modal.Open() {
		t.Fatal("N must open the note editor")
	}
	typeText(t, m, "garantie jusqu'en 2028")
	press(t, m, "ctrl+s")
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if strings.TrimSpace(string(args)) != "artefact:"+a.ID+"|perso" {
		t.Fatalf("notes.add got %q (%q)", args, m.status)
	}
	press(t, m, "enter") // detail
	if s := screen(m); !strings.Contains(s, "Notes (1)") || !strings.Contains(s, "garantie jusqu'en 2028") {
		t.Fatalf("detail without the note:\n%s", s)
	}
}
