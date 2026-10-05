package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocflgo "github.com/srerickson/ocfl-go"
	ocflfs "github.com/srerickson/ocfl-go/fs"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
)

func replicated(t *testing.T) (*Store, string) {
	t.Helper()
	s := sealedStore(t)
	remote := t.TempDir()
	s.Cfg.RcloneConfig = filepath.Join(t.TempDir(), "rclone.conf")
	s.Cfg.Backends = map[string]config.Backend{"copy": {Remote: ":local:" + remote, Sphere: "perso"}}
	s.Cfg.Replication = map[string]config.Replication{"perso": {NumCopies: 2, MinCopies: 1, Backends: []string{"copy"}}}
	return s, filepath.Join(remote, "perso")
}

func grepDir(t *testing.T, dir, needle string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(needle)) {
				found = true
			}
		}
		return nil
	})
	return found
}

func TestSyncMirrorsTheStore(t *testing.T) {
	s, dir := replicated(t)
	ctx := context.Background()
	clear := put(t, s, "CLEARCONTENT", PutOptions{Filename: "a.pdf", Title: "Clear title"})
	put(t, s, "SECRETCONTENT", PutOptions{Filename: "b.txt", Title: "Secret title", Seal: true})
	cov, _ := s.Coverage("perso")
	if cov[0].BelowNumCopies != 2 {
		t.Fatalf("before sync: %+v", cov[0])
	}
	res, err := s.Sync(ctx, "perso", false)
	if err != nil {
		t.Fatal(err)
	}
	if st := res[0].Backends[0]; st.Uploaded == 0 || len(st.Errors) > 0 {
		t.Fatalf("sync %+v", st)
	}
	again, _ := s.Sync(ctx, "perso", false)
	if again[0].Backends[0].Uploaded != 0 {
		t.Errorf("a second sync sent %d files", again[0].Backends[0].Uploaded)
	}
	cov, _ = s.Coverage("perso")
	if cov[0].BelowNumCopies != 0 {
		t.Errorf("after sync: %+v", cov[0])
	}
	if !grepDir(t, dir, "CLEARCONTENT") || !grepDir(t, dir, "Clear title") {
		t.Error("the clear artefact should be copied as is")
	}
	if grepDir(t, dir, "SECRETCONTENT") || grepDir(t, dir, "Secret title") {
		t.Error("a sealed artefact's content or card reached the backend in clear")
	}
	st, _, _ := s.Stat([]string{clear.ID}, false)
	if len(st[0].Copies) != 1 || !st[0].Copies[0].Complete {
		t.Errorf("copies %+v", st[0].Copies)
	}
	// The backend holds a valid OCFL storage root.
	ref, _ := s.Resolve(clear.ID)
	rel, _ := filepath.Rel(s.root("perso"), ref.Dir)
	v := ocflgo.ValidateObject(ctx, ocflfs.DirFS(dir), filepath.ToSlash(rel))
	for _, e := range v.Errors() {
		t.Errorf("OCFL on the backend: %v", e)
	}
}

func TestRewritesPurgeTheBackend(t *testing.T) {
	s, dir := replicated(t)
	ctx := context.Background()
	r := put(t, s, "OLDVERSION", PutOptions{Filename: "n.md"})
	if _, err := s.Sync(ctx, "perso", false); err != nil {
		t.Fatal(err)
	}
	red := filepath.Join(t.TempDir(), "r.md")
	os.WriteFile(red, []byte("NEWVERSION"), 0o644)
	res, err := s.Redact(r.ID, red, true)
	if err != nil || len(res.Backends) != 1 || len(res.Backends[0].Errors) > 0 {
		t.Fatalf("redact %+v %v", res, err)
	}
	if grepDir(t, dir, "OLDVERSION") || !grepDir(t, dir, "NEWVERSION") {
		t.Error("redact --purge must replace the backend's copy at once")
	}
	sr, err := s.Seal(r.ID)
	if err != nil || len(sr.Backends) != 1 {
		t.Fatalf("seal %+v %v", sr, err)
	}
	if grepDir(t, dir, "NEWVERSION") {
		t.Error("seal left clear text on the backend")
	}
	plan, err := s.PlanErase(EraseRequest{Refs: []string{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items[0].Locations) != 2 {
		t.Errorf("plan locations %+v", plan.Items[0].Locations)
	}
	proof, err := s.ConfirmErase(plan.Code)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof.Locations) != 2 || !proof.Locations[1].Verified || proof.Locations[1].Backend != "copy" {
		t.Errorf("proof %+v", proof.Locations)
	}
	left, _ := s.BackendFiles(ctx, "perso", "copy")
	for _, f := range left {
		if !strings.HasPrefix(f, "0=") && !strings.HasPrefix(f, "ocfl_layout") && !strings.HasPrefix(f, "extensions/0004") {
			t.Errorf("left on the backend after erase: %s", f)
		}
	}
}

func TestFsckFindsABadCopy(t *testing.T) {
	s, dir := replicated(t)
	ctx := context.Background()
	r := put(t, s, "GOOD", PutOptions{Filename: "g.txt"})
	if _, err := s.Sync(ctx, "perso", false); err != nil {
		t.Fatal(err)
	}
	ref, _ := s.Resolve(r.ID)
	rel, _ := filepath.Rel(s.root("perso"), ref.Dir)
	os.WriteFile(filepath.Join(dir, rel, "v1", "content", "payload.txt"), []byte("BAD!"), 0o644)
	res, err := s.Fsck(ctx, "perso", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].RemoteBad) != 1 || len(res[0].LocalBad) != 0 {
		t.Fatalf("fsck %+v", res[0])
	}
	if _, err := s.Sync(ctx, "perso", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, rel, "v1", "content", "payload.txt")); string(b) != "GOOD" {
		t.Errorf("sync did not repair the copy: %q", b)
	}
	res, _ = s.Fsck(ctx, "perso", 0, true)
	if len(res[0].RemoteBad) != 0 {
		t.Errorf("after repair %+v", res[0])
	}
	held, _ := s.List("perso", index.Filter{})
	if len(held) != 1 {
		t.Errorf("list %d", len(held))
	}
}

func TestSyncPublishesProgress(t *testing.T) {
	s, _ := replicated(t)
	for i := 0; i < 3; i++ {
		put(t, s, strings.Repeat("x", i+1), PutOptions{Filename: "f.txt"})
	}
	if _, err := s.Sync(context.Background(), "perso", false); err != nil {
		t.Fatal(err)
	}
	p := s.Progress()
	if len(p) != 1 || p[0].Running || p[0].Done == 0 || p[0].Done != p[0].Total {
		t.Errorf("progress %+v", p)
	}
}
