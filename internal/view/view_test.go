package view

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func TestTree(t *testing.T) {
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

	clear, err := st.Put(strings.NewReader("v1"), store.PutOptions{Filename: "scan.PDF"})
	if err != nil {
		t.Fatal(err)
	}
	red := filepath.Join(t.TempDir(), "r.pdf")
	os.WriteFile(red, []byte("v2!"), 0o644)
	if _, err := st.Redact(clear.ID, red, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EditCard(clear.ID, []string{"filename=renamed.docx"}, ""); err != nil {
		t.Fatal(err)
	}
	sealed, err := st.Put(strings.NewReader("hidden"), store.PutOptions{Filename: "s.txt", Seal: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(strings.NewReader("pro"), store.PutOptions{Filename: "p.txt", Sphere: "pro"}); err != nil {
		t.Fatal(err)
	}

	tr := New(st, "perso")
	l, err := tr.ReadDir("/a")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range l {
		names = append(names, x.Name())
	}
	want := []string{clear.ID + ".pdf", clear.ID}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("a/ = %v, want %v (sealed and pro artefacts hidden, extension fixed at deposit)", names, want)
	}
	if _, err := tr.Stat("a/" + sealed.ID + ".txt"); err == nil {
		t.Error("a sealed artefact must not be reachable")
	}
	fi, err := tr.Stat("a/" + clear.ID + ".pdf")
	if err != nil || fi.Size() != 3 {
		t.Fatalf("current version: %v %v", fi, err)
	}
	f, _, err := tr.Open("a/" + clear.ID + "/v1.pdf")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "v1" {
		t.Errorf("v1 = %q", b)
	}
	if _, err := (Billy{T: tr}).OpenFile("a/x", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		t.Error("the view must refuse writes")
	}
}

func TestArchivedHiddenButReachable(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}, DefaultSphere: "perso"}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("x"), store.PutOptions{Filename: "a.pdf"})
	if _, err := st.Archive(a.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	tr := New(st, "perso")
	l, _ := tr.ReadDir("a")
	if len(l) != 0 {
		t.Errorf("an archived artefact is listed: %v", l)
	}
	if _, err := tr.Stat("a/" + a.ID + ".pdf"); err != nil {
		t.Errorf("an archived artefact must stay reachable by its path: %v", err)
	}
}
