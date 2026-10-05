package store

import (
	"context"
	"path/filepath"
	"testing"

	ocflgo "github.com/srerickson/ocfl-go"
	ocflfs "github.com/srerickson/ocfl-go/fs"
)

// The objects artefact writes must pass an independent OCFL validator.
func TestObjectsPassOCFLValidator(t *testing.T) {
	s := newStore(t)
	r := put(t, s, "%PDF-1.7 conformance", PutOptions{Filename: "scan.pdf"})
	if _, err := s.EditCard(r.ID, []string{"title=Changed"}, ""); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Resolve(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := s.root("perso")
	rel, _ := filepath.Rel(root, ref.Dir)
	v := ocflgo.ValidateObject(context.Background(), ocflfs.DirFS(root), filepath.ToSlash(rel))
	for _, e := range v.Errors() {
		t.Errorf("OCFL error: %v", e)
	}
	for _, w := range v.WarnErrors() {
		t.Logf("OCFL warning: %v", w)
	}
}
