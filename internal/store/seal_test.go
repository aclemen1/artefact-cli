package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocflgo "github.com/srerickson/ocfl-go"
	ocflfs "github.com/srerickson/ocfl-go/fs"

	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

func sealedStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	mem := keys.Memory{}
	s.Cfg.Keys = map[string]string{}
	for _, sp := range s.Cfg.Spheres {
		id, rcp, err := keys.Generate()
		if err != nil {
			t.Fatal(err)
		}
		mem[sp], s.Cfg.Keys[sp] = id, rcp
	}
	s.Keys = mem
	return s
}

func read(t *testing.T, s *Store, ref, v string, reveal bool) string {
	t.Helper()
	rc, _, err := s.OpenPayload(ref, v, reveal)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func validate(t *testing.T, s *Store, ref string) {
	t.Helper()
	r, err := s.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	root := s.root(r.Sphere)
	rel, _ := filepath.Rel(root, r.Dir)
	v := ocflgo.ValidateObject(context.Background(), ocflfs.DirFS(root), filepath.ToSlash(rel))
	for _, e := range v.Errors() {
		t.Errorf("OCFL error: %v", e)
	}
}

// grepStore reports whether needle appears in any file of the sphere's store.
func grepStore(t *testing.T, s *Store, sphere, needle string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(s.root(sphere), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(needle)) {
				found = true
			}
		}
		return nil
	})
	return found
}

func TestPutSealed(t *testing.T) {
	s := sealedStore(t)
	secret := "PIN 4512 for the card"
	r := put(t, s, secret, PutOptions{Sphere: "perso", Filename: "nip.txt", Seal: true})
	if !r.Sealed || r.Size != int64(len(secret)) {
		t.Fatalf("result %+v", r)
	}
	if grepStore(t, s, "perso", "4512") {
		t.Fatal("the plaintext is in the store")
	}
	if _, _, err := s.OpenPayload(r.ID, "", false); err == nil {
		t.Error("a sealed artefact must not be read without reveal")
	}
	if got := read(t, s, r.ID, "", true); got != secret {
		t.Errorf("revealed %q", got)
	}
	again := put(t, s, secret, PutOptions{Sphere: "perso", Filename: "other.txt", Seal: true, Holds: []index.Holder{{By: "mnemo", Ref: "C9"}}})
	if again.Status != "known" || again.ID != r.ID {
		t.Errorf("same sealed payload: %+v", again)
	}
	clear := put(t, s, secret, PutOptions{Sphere: "perso", Filename: "nip.txt"})
	if clear.ID == r.ID {
		t.Error("a clear deposit must not match a sealed artefact")
	}
	k1 := put(t, s, "abc", PutOptions{Sphere: "perso", Seal: true, Key: "K"})
	k2 := put(t, s, "abc", PutOptions{Sphere: "perso", Seal: true, Key: "K"})
	if k1.ID != k2.ID {
		t.Error("idempotency key on a sealed payload")
	}
	validate(t, s, r.ID)
	st, _, _ := s.Stat([]string{r.ID}, false)
	if st[0].Card.Keyed == "" || strings.Contains(st[0].Card.Keyed, "4512") {
		t.Errorf("keyed digest %q", st[0].Card.Keyed)
	}
}

func TestSealErasesClearVersions(t *testing.T) {
	s := sealedStore(t)
	r := put(t, s, "clear v1 TOPSECRET", PutOptions{Sphere: "perso", Filename: "codes.txt", Key: "K1", Holds: []index.Holder{{By: "mnemo", Ref: "C"}}})
	tmp := filepath.Join(t.TempDir(), "v2.txt")
	os.WriteFile(tmp, []byte("clear v2 TOPSECRET"), 0o644)
	if _, err := s.Redact(r.ID, tmp, false); err != nil {
		t.Fatal(err)
	}
	res, err := s.Seal(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "sealed" || res.VersionsErased != 2 {
		t.Fatalf("seal %+v", res)
	}
	if grepStore(t, s, "perso", "TOPSECRET") {
		t.Fatal("clear text left in the store after seal")
	}
	if got := read(t, s, r.ID, "", true); got != "clear v2 TOPSECRET" {
		t.Errorf("revealed %q", got)
	}
	st, _, _ := s.Stat([]string{r.ID}, true)
	if len(st[0].Versions) != 1 || !st[0].Card.Sealed || len(st[0].Card.Holders) != 1 {
		t.Errorf("after seal: versions %d card %+v", len(st[0].Versions), st[0].Card)
	}
	if res, _ := s.Seal(r.ID); res.Status != "already_sealed" {
		t.Errorf("second seal: %s", res.Status)
	}
	validate(t, s, r.ID)
	if _, err := s.Reindex("perso"); err != nil {
		t.Fatal(err)
	}
	again := put(t, s, "clear v2 TOPSECRET", PutOptions{Sphere: "perso", Seal: true})
	if again.ID != r.ID {
		t.Error("sealed artefact not found by its keyed digest after reindex")
	}
}

func TestRedact(t *testing.T) {
	s := sealedStore(t)
	r := put(t, s, "name: Jean, AVS 756.1234", PutOptions{Sphere: "perso", Filename: "note.md"})
	tmp := filepath.Join(t.TempDir(), "red.md")
	os.WriteFile(tmp, []byte("name: Jean, AVS •••"), 0o644)
	res, err := s.Redact(r.ID, tmp, false)
	if err != nil || res.Version != 2 || res.VersionsErased != 0 {
		t.Fatalf("redact %+v %v", res, err)
	}
	if read(t, s, r.ID, "v1", false) != "name: Jean, AVS 756.1234" || read(t, s, r.ID, "", false) != "name: Jean, AVS •••" {
		t.Error("versions after redact")
	}
	validate(t, s, r.ID)
	os.WriteFile(tmp, []byte("name: •••"), 0o644)
	res, err = s.Redact(r.ID, tmp, true)
	if err != nil || res.Version != 1 || res.VersionsErased != 2 {
		t.Fatalf("redact --purge %+v %v", res, err)
	}
	if grepStore(t, s, "perso", "756.1234") {
		t.Error("purged text still in the store")
	}
	// The redacted file was copied, not linked: changing it later does not touch the store.
	os.WriteFile(tmp, []byte("changed"), 0o644)
	if read(t, s, r.ID, "", false) != "name: •••" {
		t.Error("the store shares the redacted file with the caller")
	}
	validate(t, s, r.ID)
}

func TestEraseWhole(t *testing.T) {
	s := sealedStore(t)
	a := put(t, s, "erase me", PutOptions{Sphere: "perso", Filename: "a.txt", Holds: []index.Holder{{By: "mnemo", Ref: "C"}}})
	b := put(t, s, "keep me", PutOptions{Sphere: "perso", Filename: "b.txt"})
	var ue *UserError
	if _, err := s.PlanErase(EraseRequest{Refs: []string{a.ID}}); !errors.As(err, &ue) {
		t.Fatalf("a held artefact must need --force, got %v", err)
	}
	if _, _, err := s.Release(a.ID, "mnemo", ""); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanErase(EraseRequest{Refs: []string{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || !plan.Items[0].Whole || plan.Code == "" {
		t.Fatalf("plan %+v", plan)
	}
	ref, _ := s.Resolve(a.ID)
	proof, err := s.ConfirmErase(plan.Code)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof.Erased) != 1 || !proof.Locations[0].Verified {
		t.Errorf("proof %+v", proof)
	}
	if _, err := os.Stat(ref.Dir); !os.IsNotExist(err) {
		t.Error("object still on disk")
	}
	var nf *NotFound
	if _, err := s.Resolve(a.ID); !errors.As(err, &nf) {
		t.Error("erased artefact still resolves")
	}
	if !s.Erased("perso", a.ID) {
		t.Error("no tombstone")
	}
	if grepStore(t, s, "perso", "erase me") {
		t.Error("erased content left in the store")
	}
	if _, err := s.ConfirmErase(plan.Code); err == nil {
		t.Error("a code must be single-use")
	}
	if _, err := s.Resolve(b.ID); err != nil {
		t.Error("the other artefact was touched")
	}
}

func TestEraseVersionAndChangedPlan(t *testing.T) {
	s := sealedStore(t)
	r := put(t, s, "v1 PRIVATE", PutOptions{Sphere: "perso", Filename: "n.txt"})
	tmp := filepath.Join(t.TempDir(), "v2.txt")
	os.WriteFile(tmp, []byte("v2 public"), 0o644)
	if _, err := s.Redact(r.ID, tmp, false); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanErase(EraseRequest{Refs: []string{r.ID}, Version: "v1"})
	if err != nil || plan.Items[0].Whole {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// The artefact changes between plan and confirmation: nothing is erased.
	if _, err := s.Hold(r.ID, "mnemo", "C"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmErase(plan.Code); err == nil {
		t.Fatal("a changed artefact must refuse the old plan")
	}
	plan, err = s.PlanErase(EraseRequest{Refs: []string{r.ID}, Version: "v1", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmErase(plan.Code); err != nil {
		t.Fatal(err)
	}
	st, _, _ := s.Stat([]string{r.ID}, true)
	if len(st[0].Versions) != 1 || st[0].Card.Version != 1 || read(t, s, r.ID, "", false) != "v2 public" {
		t.Errorf("after erasing v1: %+v", st[0])
	}
	if grepStore(t, s, "perso", "PRIVATE") {
		t.Error("erased version left in the store")
	}
	validate(t, s, r.ID)
}

func TestEraseCodeExpires(t *testing.T) {
	s := sealedStore(t)
	r := put(t, s, "x", PutOptions{Sphere: "perso", Filename: "x.txt"})
	plan, err := s.PlanErase(EraseRequest{Refs: []string{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, err := s.ConfirmErase(plan.Code); err == nil {
		t.Error("an expired code must be refused")
	}
	if _, err := s.Resolve(r.ID); err != nil {
		t.Error("an expired code erased the artefact")
	}
}

func TestRecoverPutsBackAnInterruptedSwap(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "survive", PutOptions{Sphere: "perso", Filename: "s.txt"})
	ref, _ := s.Resolve(r.ID)
	root := s.root("perso")
	// Simulate a crash between the two renames of a rebuild.
	trash := filepath.Join(root, "extensions", "artefact-staging", "trash-test")
	rel, _ := filepath.Rel(root, ref.Dir)
	os.WriteFile(trash+".target", []byte(rel), 0o644)
	if err := os.Rename(ref.Dir, trash); err != nil {
		t.Fatal(err)
	}
	if err := ocfl.Recover(root); err != nil {
		t.Fatal(err)
	}
	if read(t, s, r.ID, "", false) != "survive" {
		t.Error("object not put back")
	}
}
