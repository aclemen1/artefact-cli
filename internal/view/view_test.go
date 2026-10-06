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

	clear, err := st.Put(strings.NewReader("v1"), store.PutOptions{Sphere: "perso", Filename: "scan.PDF"})
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
	sealed, err := st.Put(strings.NewReader("hidden"), store.PutOptions{Sphere: "perso", Filename: "s.txt", Seal: true})
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
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	a, _ := st.Put(strings.NewReader("x"), store.PutOptions{Sphere: "perso", Filename: "a.pdf"})
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

func TestPlansView(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	plan := "id: household\ntitle: H\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n"
	if _, err := st.Plans().Save([]byte(plan), false); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Put(strings.NewReader("aaa"), store.PutOptions{Sphere: "perso", Filename: "x.pdf", Title: "Décompte: Assura"})
	b, _ := st.Put(strings.NewReader("bbb"), store.PutOptions{Sphere: "perso", Filename: "y.pdf", Title: "Décompte: Assura"})
	st.Put(strings.NewReader("ccc"), store.PutOptions{Sphere: "perso", Filename: "z.txt", Title: "Note"})
	st.SetClass(a.ID, "household", "3.2", "alain")
	st.SetClass(b.ID, "household", "3.2", "alain")
	tr := New(st, "perso")
	names := func(p string) []string {
		l, err := tr.ReadDir(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		var out []string
		for _, x := range l {
			out = append(out, x.Name())
		}
		return out
	}
	if got := strings.Join(names("plans/household"), ","); got != "3 Health,_unclassified" {
		t.Errorf("plan root: %s", got)
	}
	got := names("plans/household/3 Health/3.2 Insurance")
	if len(got) != 2 || got[0] != "Décompte- Assura ("+b.ID[20:]+").pdf" && got[1] != "Décompte- Assura ("+b.ID[20:]+").pdf" {
		t.Errorf("node files: %v", got)
	}
	if got := names("plans/household/_unclassified"); len(got) != 1 || got[0] != "Note.txt" {
		t.Errorf("unclassified: %v", got)
	}
	f, _, err := tr.Open("plans/household/3 Health/3.2 Insurance/Décompte- Assura.pdf")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	f.Read(buf)
	f.Close()
	if string(buf) != "aaa" && string(buf) != "bbb" {
		t.Errorf("content %q", buf)
	}
}

func TestNoExtensionKeepsFileAndVersionsApart(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	r, err := st.Put(strings.NewReader("%PDF bordereau"), store.PutOptions{Sphere: "perso", Filename: "Bordereau - PROJET", Mime: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	tr := New(st, "perso")
	fi, err := tr.Stat("a/" + r.ID)
	if err != nil || fi.IsDir() {
		t.Fatalf("a/<id> must be the current file: %+v %v", fi, err)
	}
	if f, _, err := tr.Open("a/" + r.ID); err != nil {
		t.Fatal(err)
	} else {
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != "%PDF bordereau" {
			t.Fatalf("content %q", b)
		}
	}
	d, err := tr.Stat("a/" + r.ID + ".versions")
	if err != nil || !d.IsDir() {
		t.Fatalf("versions directory: %+v %v", d, err)
	}
	vs, err := tr.ReadDir("a/" + r.ID + ".versions")
	if err != nil || len(vs) != 1 || vs[0].Name() != "v1" {
		t.Fatalf("versions: %v %v", vs, err)
	}
	names := map[string]bool{}
	list, _ := tr.ReadDir("a")
	for _, e := range list {
		if names[e.Name()] {
			t.Fatalf("a/ lists %s twice", e.Name())
		}
		names[e.Name()] = true
	}
	if !names[r.ID] || !names[r.ID+".versions"] {
		t.Fatalf("a/ lists %v", names)
	}
}

func TestPlanPath(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"3\"\n    label: Health\n    children:\n      - notation: \"3.2\"\n        label: Insurance\n"), false)
	a, _ := st.Put(strings.NewReader("a"), store.PutOptions{Sphere: "perso", Filename: "a.pdf", Title: "Assura"})
	b, _ := st.Put(strings.NewReader("b"), store.PutOptions{Sphere: "perso", Filename: "b.pdf", Title: "Bail"})
	st.SetClass(a.ID, "h", "3.2", "alain")
	tr := New(st, "perso")
	if p, ok := tr.PlanPath("h", a.ID); !ok || p != "plans/h/3 Health/3.2 Insurance/Assura.pdf" {
		t.Errorf("classified: %q %v", p, ok)
	}
	if p, ok := tr.PlanPath("h", b.ID); !ok || p != "plans/h/_unclassified/Bail.pdf" {
		t.Errorf("unclassified: %q %v", p, ok)
	}
	if fi, err := tr.Stat("plans/h/3 Health/3.2 Insurance/Assura.pdf"); err != nil || fi.IsDir() {
		t.Errorf("the path must exist in the view: %v", err)
	}
}

func TestDecomposedNamesResolve(t *testing.T) {
	cfg := &config.Config{Store: t.TempDir(), Spheres: []string{"perso"}}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	st.Init()
	st.Plans().Save([]byte("id: h\ntitle: Household\nsphere: perso\nnodes:\n  - notation: \"1\"\n    label: Identité\n"), false)
	a, _ := st.Put(strings.NewReader("x"), store.PutOptions{Sphere: "perso", Filename: "l.pdf", Title: "Livret Clément"})
	st.SetClass(a.ID, "h", "1", "alain")
	tr := New(st, "perso")
	nfd := "plans/h/1 Identité/Livret Clément.pdf" // as macOS sends it
	if fi, err := tr.Stat(nfd); err != nil || fi.IsDir() {
		t.Fatalf("decomposed path: %v", err)
	}
	if f, _, err := tr.Open(nfd); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
}
