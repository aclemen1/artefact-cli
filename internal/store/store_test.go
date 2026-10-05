package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

func newStore(t *testing.T, spheres ...string) *Store {
	t.Helper()
	if len(spheres) == 0 {
		spheres = []string{"perso", "pro"}
	}
	cfg := &config.Config{Store: t.TempDir(), Spheres: spheres, DefaultSphere: spheres[0]}
	s, err := Open(cfg, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func put(t *testing.T, s *Store, content string, o PutOptions) *PutResult {
	t.Helper()
	r, err := s.Put(strings.NewReader(content), o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPutCreatesValidObject(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "%PDF-1.7 hello", PutOptions{Filename: "ordonnance-2026-09.pdf", Date: "2026-09-18"})
	if r.Status != "created" || r.Version != 1 || r.Sphere != "perso" {
		t.Fatalf("unexpected result %+v", r)
	}
	if r.Title != "ordonnance-2026-09" {
		t.Errorf("title = %q", r.Title)
	}
	ref, err := s.Resolve(r.URI)
	if err != nil {
		t.Fatal(err)
	}
	if err := ocfl.Verify(ref.Dir); err != nil {
		t.Fatal(err)
	}
	inv, err := ocfl.ReadInventory(ref.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if inv.ID != r.URI || inv.Head != "v1" {
		t.Errorf("inventory id %q head %q", inv.ID, inv.Head)
	}
	if filepath.Base(ref.Dir) == r.ID {
		t.Errorf("object root should be named by the hash of the id")
	}
	c, err := readCard(ref.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mime != "application/pdf" || c.Date != "2026-09-18" || c.Digest != r.Digest {
		t.Errorf("card %+v", c)
	}
	if _, err := os.Stat(filepath.Join(s.root("perso"), "extensions", "artefact-staging")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(s.root("perso"), "extensions", "artefact-staging"))
		if len(entries) != 0 {
			t.Errorf("staging area not empty: %d entries", len(entries))
		}
	}
}

func TestPutIsIdempotentWithinSphere(t *testing.T) {
	s := newStore(t)
	a := put(t, s, "same bytes", PutOptions{Filename: "a.txt", Holds: []index.Holder{{By: "mnemo", Ref: "C1"}}})
	b := put(t, s, "same bytes", PutOptions{Filename: "b.txt", Holds: []index.Holder{{By: "mnemo", Ref: "C2"}}})
	if b.Status != "known" || b.ID != a.ID {
		t.Fatalf("second put = %+v, want known %s", b, a.ID)
	}
	if len(b.Holders) != 2 {
		t.Errorf("holders = %+v, want C1 and C2", b.Holders)
	}
	c := put(t, s, "same bytes", PutOptions{Sphere: "pro", Filename: "a.txt"})
	if c.Status != "created" || c.ID == a.ID {
		t.Errorf("a payload in another sphere must be a new artefact, got %+v", c)
	}
}

func TestPutKey(t *testing.T) {
	s := newStore(t)
	a := put(t, s, "v1", PutOptions{Filename: "x.txt", Key: "K"})
	b := put(t, s, "v1", PutOptions{Filename: "x.txt", Key: "K"})
	if b.ID != a.ID || b.Status != "known" {
		t.Fatalf("replay = %+v", b)
	}
	_, err := s.Put(strings.NewReader("other"), PutOptions{Filename: "x.txt", Key: "K"})
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("a key reused for another payload must fail, got %v", err)
	}
	if _, err := s.Reindex(""); err != nil {
		t.Fatal(err)
	}
	c := put(t, s, "v1", PutOptions{Filename: "x.txt", Key: "K"})
	if c.ID != a.ID {
		t.Errorf("key lost by reindex")
	}
}

func TestHoldReleaseIdempotent(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "x", PutOptions{Filename: "x.bin"})
	for range 2 {
		if _, err := s.Hold(r.ID, "mnemo", "C1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Hold(r.ID, "mnemo", "C2"); err != nil {
		t.Fatal(err)
	}
	held, _ := s.List("", index.Filter{HeldBy: "mnemo:C1"})
	if len(held) != 1 {
		t.Errorf("held by mnemo:C1 = %d", len(held))
	}
	c, n, err := s.Release(r.ID, "mnemo", "C1")
	if err != nil || n != 1 || len(c.Holders) != 1 {
		t.Fatalf("release C1: n=%d holders=%v err=%v", n, c.Holders, err)
	}
	if _, n, _ = s.Release(r.ID, "mnemo", "C1"); n != 0 {
		t.Errorf("releasing an absent holder released %d", n)
	}
	if _, n, _ = s.Release(r.ID, "mnemo", ""); n != 1 {
		t.Errorf("release all of mnemo released %d", n)
	}
	orphans, _ := s.List("perso", index.Filter{Orphaned: true})
	if len(orphans) != 1 {
		t.Errorf("orphaned = %d", len(orphans))
	}
}

func TestCardEditAndReindex(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "x", PutOptions{Filename: "scan.png"})
	if _, err := s.EditCard(r.ID, []string{"title=Ordonnance", "dc.creator=Dr X", "x.mnemo.note=AVS"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditCard(r.ID, []string{"digest=sha256:00"}, ""); err == nil {
		t.Error("digest must not be editable")
	}
	if _, err := s.EditCard(r.ID, []string{"date=18.09.2026"}, ""); err == nil {
		t.Error("a date that is not YYYY-MM-DD must be refused")
	}
	if _, err := s.Reindex("perso"); err != nil {
		t.Fatal(err)
	}
	st, missing, err := s.Stat([]string{r.ID, "01K9B3T0Z6Q8M2C4R7V1X5N9PA"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || len(missing) != 1 {
		t.Fatalf("stat %d, missing %v", len(st), missing)
	}
	c := st[0].Card
	if c.Title != "Ordonnance" || c.DC["creator"] != "Dr X" || c.X["mnemo"].(map[string]any)["note"] != "AVS" {
		t.Errorf("card after reindex: %+v", c)
	}
	if len(st[0].Events) != 2 || st[0].Events[1].Type != "card" {
		t.Errorf("events = %+v", st[0].Events)
	}
}

func TestGetAndSpheres(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "payload bytes", PutOptions{Sphere: "pro", Filename: "f.txt"})
	rc, _, err := s.OpenPayload(r.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, []byte("payload bytes")) {
		t.Errorf("payload = %q", b)
	}
	if _, _, err := s.OpenPayload(r.ID, "v2", false); err == nil {
		t.Error("v2 does not exist")
	}

	persoOnly, err := Open(s.Cfg, []string{"perso"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer persoOnly.Close()
	var nf *NotFound
	if _, err := persoOnly.Resolve(r.ID); !errors.As(err, &nf) {
		t.Errorf("a pro artefact must be invisible to a perso-only store, got %v", err)
	}
	var ue *UserError
	if _, err := persoOnly.Resolve(r.URI); !errors.As(err, &ue) {
		t.Errorf("a pro URI must be refused explicitly, got %v", err)
	}
	if _, err := persoOnly.Put(strings.NewReader("x"), PutOptions{Sphere: "pro"}); !errors.As(err, &ue) {
		t.Errorf("a deposit into an ungranted sphere must be refused, got %v", err)
	}
}

func TestPutFromStdinWithoutName(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "plain text\n", PutOptions{})
	if r.Title != r.ID {
		t.Errorf("title = %q, want the id", r.Title)
	}
	st, _, _ := s.Stat([]string{r.ID}, false)
	if st[0].Card.Mime != "text/plain" {
		t.Errorf("mime = %q", st[0].Card.Mime)
	}
}

func TestUsage(t *testing.T) {
	s := newStore(t)
	put(t, s, "12345", PutOptions{Filename: "a.txt", Holds: []index.Holder{{By: "mnemo", Ref: "C"}}})
	put(t, s, "678", PutOptions{Filename: "b.txt"})
	u, err := s.Usage("perso")
	if err != nil {
		t.Fatal(err)
	}
	if u[0].Artefacts != 2 || u[0].Bytes != 8 || u[0].Orphaned != 1 || u[0].DiskBytes == 0 {
		t.Errorf("usage = %+v", u[0])
	}
}
