package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func TestPrefsSurviveARestart(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso", "pro"}, DefaultSphere: "perso"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n"), false)
	st.Put(strings.NewReader("a"), store.PutOptions{Filename: "a.pdf"})
	path := filepath.Join(t.TempDir(), "tui.json")
	locate := func(*config.Config, *store.Store, string) (string, string, error) { return "", "", nil }

	m := newModel(cfg, st, locate)
	m.loadPrefs(path)
	m.w, m.h = 120, 30
	drive(t, m, m.Init())
	press(t, m, "s") // perso
	press(t, m, "t") // date
	press(t, m, "T") // date ascending
	press(t, m, "C")
	press(t, m, "j") // 3 Health
	press(t, m, "l") // open

	n := newModel(cfg, st, locate)
	n.loadPrefs(path)
	if n.sphere != "perso" || n.order.String() != "date:asc" || !n.treeOpen["h:3"] {
		t.Fatalf("restored: sphere %q, sort %s, open %v", n.sphere, n.order, n.treeOpen)
	}
	n.w, n.h = 120, 30
	drive(t, n, n.Init())
	press(t, n, "C")
	if len(n.planView.rows) != 4 { // Household, 3, 3.2, unclassified
		t.Fatalf("tree not reopened: %+v", n.planView.rows)
	}
}
