package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/meta"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

// versionsOf turns the versions of an object into sources for a rebuild.
func versionsOf(objDir string, inv *ocfl.Inventory, keep func(v string) bool) ([]ocfl.NewVersion, map[int]int, error) {
	var out []ocfl.NewVersion
	renumber := map[int]int{}
	for i, name := range inv.VersionNames() {
		if !keep(name) {
			continue
		}
		files, err := inv.Files(name)
		if err != nil {
			return nil, nil, err
		}
		v := inv.Versions[name]
		created, _ := time.Parse(time.RFC3339, v.Created)
		nv := ocfl.NewVersion{Created: created, Message: v.Message, User: v.User}
		for _, f := range files {
			nv.Files = append(nv.Files, ocfl.SourceFile{Logical: f.Logical, Path: ocfl.ContentPath(objDir, f)})
		}
		out = append(out, nv)
		renumber[i+1] = len(out)
	}
	return out, renumber, nil
}

// headDigest is the SHA-256 of the head payload of inv.
func headDigest(inv *ocfl.Inventory) string {
	files, _ := inv.Files("")
	for _, f := range files {
		if strings.HasPrefix(f.Logical, "payload") {
			return "sha256:" + f.SHA256
		}
	}
	return ""
}

// renumberEvents renumbers the events of kept versions; the events of erased
// versions lose their version number (they name no content, and a deposit
// event keeps the idempotency key).
func renumberEvents(objDir string, renumber map[int]int) error {
	evs, err := readEvents(objDir)
	if err != nil {
		return err
	}
	var kept []Event
	for _, e := range evs {
		if e.Version > 0 {
			e.Version = renumber[e.Version]
		}
		kept = append(kept, e)
	}
	p := filepath.Join(objDir, extDir, eventFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		return err
	}
	for _, e := range kept {
		if err := appendEventTo(tmp, e); err != nil {
			return err
		}
	}
	return os.Rename(tmp, p)
}

// stageFile copies r into the staging area of root (sealed for recipient
// when given) and returns its path, the plaintext size and keyed digest.
func (s *Store) stageFile(sphere string, r io.Reader, seal bool) (path string, size int64, keyed string, err error) {
	dir := filepath.Join(s.root(sphere), "extensions", "artefact-staging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, "", err
	}
	f, err := os.CreateTemp(dir, "file-")
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	if !seal {
		size, err = io.Copy(f, r)
		return f.Name(), size, "", err
	}
	sl, err := s.seal(sphere, r)
	if err != nil {
		os.Remove(f.Name())
		return "", 0, "", err
	}
	_, err = io.Copy(f, sl.r)
	sl.r.CloseWithError(err)
	<-sl.done
	if err != nil {
		os.Remove(f.Name())
		return "", 0, "", err
	}
	return f.Name(), sl.size, sl.keyed(), nil
}

type SealResult struct {
	ID             string      `json:"id"`
	URI            string      `json:"uri"`
	Status         string      `json:"status"` // sealed | already_sealed
	VersionsErased int         `json:"versions_erased"`
	Digest         string      `json:"digest"`
	Backends       []SyncStats `json:"backends,omitempty"`
	ClearRemainsIn []string    `json:"clear_remains_in"`
}

// pendingBackends lists the backends a sync could not bring up to date.
func pendingBackends(stats []SyncStats) []string {
	var out []string
	for _, st := range stats {
		if len(st.Errors) > 0 {
			out = append(out, "backend "+st.Backend+" (retried at the next sync)")
		}
	}
	return out
}

// notCovered lists the copies erasure and sealing cannot reach.
var notCovered = []string{
	"backups made outside artefact (Time Machine, local APFS snapshots)",
	"copies made by holders or exported with get",
}

// Seal replaces an artefact by a single sealed version and erases its clear
// versions from the store.
func (s *Store) Seal(ref string) (*SealResult, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	res := &SealResult{ID: c.ID, URI: URI(r.Sphere, c.ID), Digest: c.Digest, ClearRemainsIn: notCovered}
	if c.Sealed {
		res.Status = "already_sealed"
		return res, nil
	}
	inv, err := ocfl.ReadInventory(r.Dir)
	if err != nil {
		return nil, err
	}
	src, err := payloadFile(r.Dir, "")
	if err != nil {
		return nil, err
	}
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	tmp, size, keyed, err := s.stageFile(r.Sphere, in, true)
	in.Close()
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	files, _ := inv.Files("")
	logical := "payload.age"
	for _, f := range files {
		if strings.HasPrefix(f.Logical, "payload") {
			logical = f.Logical + ".age"
		}
	}
	now := s.Now()
	newInv, err := ocfl.Rebuild(s.root(r.Sphere), r.Dir, []ocfl.NewVersion{{
		Files: []ocfl.SourceFile{{Logical: logical, Path: tmp}}, Created: now, Message: "seal", User: s.user(),
	}})
	if err != nil {
		return nil, err
	}
	n := len(inv.VersionNames())
	if err := renumberEvents(r.Dir, map[int]int{}); err != nil {
		return nil, err
	}
	at := now.Format(time.RFC3339)
	c.Sealed, c.Keyed, c.Size, c.Version, c.Digest = true, keyed, size, 1, headDigest(newInv)
	if err := dropRenditions(r.Dir, c); err != nil {
		return nil, err
	}
	if err := appendEvent(r.Dir, Event{At: at, Type: "seal", Agent: s.Agent, By: s.By, Outcome: "ok", Version: 1, Detail: map[string]any{"versions_erased": n}}); err != nil {
		return nil, err
	}
	if err := s.saveCard(r, c, at); err != nil {
		return nil, err
	}
	res.Status, res.VersionsErased, res.Digest = "sealed", n, c.Digest
	res.Backends = s.syncObject(context.Background(), r.Sphere, r.Row.Path)
	res.ClearRemainsIn = append(append([]string{}, notCovered...), pendingBackends(res.Backends)...)
	return res, nil
}

type RedactResult struct {
	ID             string      `json:"id"`
	URI            string      `json:"uri"`
	Version        int         `json:"version"`
	Digest         string      `json:"digest"`
	VersionsErased int         `json:"versions_erased"`
	Backends       []SyncStats `json:"backends,omitempty"`
}

// Redact adds a version made from file; with purge, the earlier versions
// are erased from the store.
func (s *Store) Redact(ref, file string, purge bool) (*RedactResult, error) {
	return s.newVersion(ref, file, purge, "redact")
}

// Revise adds a version made from file and keeps the earlier ones: an
// edit of the content.
func (s *Store) Revise(ref, file string) (*RedactResult, error) {
	return s.newVersion(ref, file, false, "revise")
}

func (s *Store) newVersion(ref, file string, purge bool, kind string) (*RedactResult, error) {
	if file == "" {
		return nil, userErr("--from names the new content. Example: artefact %s 01K9B3T0Z6Q8M2C4R7V1X5N9PA --from new.pdf", kind)
	}
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	in, err := os.Open(config.Expand(file))
	if err != nil {
		return nil, userErr("cannot read %q: %v", file, err)
	}
	defer in.Close()
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	inv, err := ocfl.ReadInventory(r.Dir)
	if err != nil {
		return nil, err
	}
	tmp, size, keyed, err := s.stageFile(r.Sphere, in, c.Sealed)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	keep := func(string) bool { return !purge }
	versions, renumber, err := versionsOf(r.Dir, inv, keep)
	if err != nil {
		return nil, err
	}
	logical := "payload"
	if files, _ := inv.Files(""); len(files) > 0 {
		for _, f := range files {
			if strings.HasPrefix(f.Logical, "payload") {
				logical = f.Logical
			}
		}
	}
	now := s.Now()
	versions = append(versions, ocfl.NewVersion{Files: []ocfl.SourceFile{{Logical: logical, Path: tmp}}, Created: now, Message: kind, User: s.user()})
	newInv, err := ocfl.Rebuild(s.root(r.Sphere), r.Dir, versions)
	if err != nil {
		return nil, err
	}
	erased := len(inv.VersionNames()) + 1 - len(versions)
	if purge {
		if err := renumberEvents(r.Dir, renumber); err != nil {
			return nil, err
		}
	}
	at := now.Format(time.RFC3339)
	c.Version, c.Digest, c.Size = len(versions), headDigest(newInv), size
	if err := dropRenditions(r.Dir, c); err != nil {
		return nil, err
	}
	if c.Sealed {
		c.Keyed = keyed
	}
	if !c.Sealed && meta.Available() {
		if e, err := meta.Read(tmp); err == nil && !e.Empty() {
			c.Embedded = &e
		}
	}
	if err := appendEvent(r.Dir, Event{At: at, Type: kind, Agent: s.Agent, By: s.By, Outcome: "ok", Version: c.Version, Detail: map[string]any{"versions_erased": erased}}); err != nil {
		return nil, err
	}
	if err := s.saveCard(r, c, at); err != nil {
		return nil, err
	}
	res := &RedactResult{ID: c.ID, URI: URI(r.Sphere, c.ID), Version: c.Version, Digest: c.Digest, VersionsErased: erased}
	if purge {
		res.Backends = s.syncObject(context.Background(), r.Sphere, r.Row.Path)
	}
	return res, nil
}
