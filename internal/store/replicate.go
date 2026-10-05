package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aclemen1/artefact-cli/internal/backend"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

// Replication copies every file of a sphere's storage root to the sphere's
// backends, so that each backend holds a valid OCFL storage root too. The
// card and event log of a sealed artefact travel encrypted.

var backendMu sync.Mutex

func (s *Store) backendsOf(ctx context.Context, sphere string) ([]*backend.Backend, error) {
	pol := s.Cfg.ReplicationOf(sphere)
	if len(pol.Backends) == 0 {
		return nil, nil
	}
	backend.Init(s.RcloneConfig())
	backendMu.Lock()
	defer backendMu.Unlock()
	if s.backends == nil {
		s.backends = map[string]*backend.Backend{}
	}
	var out []*backend.Backend
	for _, name := range pol.Backends {
		if b, ok := s.backends[name]; ok {
			out = append(out, b)
			continue
		}
		cfg := s.Cfg.Backends[name]
		b, err := backend.Open(ctx, name, strings.TrimRight(cfg.Remote, "/")+"/"+sphere)
		if err != nil {
			return nil, err
		}
		s.backends[name] = b
		out = append(out, b)
	}
	return out, nil
}

// RcloneConfig is the rclone configuration file artefact uses.
func (s *Store) RcloneConfig() string {
	if s.Cfg.RcloneConfig != "" {
		return s.Cfg.RcloneConfig
	}
	return backend.ConfigPath()
}

type localFile struct {
	rel   string
	abs   string
	size  int64
	mtime int64
}

// localFiles lists the files of the sphere's storage root under prefix,
// staging area excluded.
func (s *Store) localFiles(sphere, prefix string) ([]localFile, error) {
	root := s.root(sphere)
	start := filepath.Join(root, filepath.FromSlash(prefix))
	var out []localFile
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "extensions/artefact-staging" {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, localFile{rel: rel, abs: p, size: fi.Size(), mtime: fi.ModTime().UnixNano()})
		return nil
	})
	return out, err
}

func (s *Store) localHash(x *index.Index, f localFile) (string, error) {
	if h, ok := x.LocalHash(f.rel, f.size, f.mtime); ok {
		return h, nil
	}
	r, err := os.Open(f.abs)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return sum, x.PutLocalHash(f.rel, f.size, f.mtime, sum)
}

// sealedObjects maps the object roots of the sphere's sealed artefacts.
func sealedObjects(x *index.Index) (map[string]bool, error) {
	rows, err := x.List(index.Filter{})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Sealed {
			out[filepath.ToSlash(r.Path)] = true
		}
	}
	return out, nil
}

// encrypted reports whether rel is the card or event log of a sealed object.
func encrypted(rel string, sealed map[string]bool) bool {
	base := filepath.Base(rel)
	if base != cardFile && base != eventFile {
		return false
	}
	obj := strings.TrimSuffix(rel, "/"+extDir+"/"+base)
	return sealed[obj]
}

// SyncStats counts what one sync did on one backend.
type SyncStats struct {
	Backend  string   `json:"backend"`
	Uploaded int      `json:"uploaded"`
	Bytes    int64    `json:"bytes"`
	Deleted  int      `json:"deleted"`
	Errors   []string `json:"errors,omitempty"`
}

// syncPrefix makes backend b hold exactly the local files under prefix.
func (s *Store) syncPrefix(ctx context.Context, sphere string, b *backend.Backend, prefix string, dry bool) SyncStats {
	st := SyncStats{Backend: b.Name}
	fail := func(err error) { st.Errors = append(st.Errors, err.Error()) }
	x, err := s.index(sphere)
	if err != nil {
		fail(err)
		return st
	}
	locals, err := s.localFiles(sphere, prefix)
	if err != nil {
		fail(err)
		return st
	}
	sealed, err := sealedObjects(x)
	if err != nil {
		fail(err)
		return st
	}
	recs, err := x.Remotes(b.Name, prefix)
	if err != nil {
		fail(err)
		return st
	}
	have := map[string]index.RemoteFile{}
	for _, r := range recs {
		have[r.Path] = r
	}
	seen := map[string]bool{}
	recipient := ""
	for _, f := range locals {
		seen[f.rel] = true
		sum, err := s.localHash(x, f)
		if err != nil {
			fail(err)
			continue
		}
		enc := encrypted(f.rel, sealed)
		remote := f.rel
		if enc {
			remote += ".age"
		}
		if r, ok := have[f.rel]; ok && r.SHA256 == sum && r.Remote == remote {
			continue
		}
		if dry {
			st.Uploaded++
			st.Bytes += f.size
			continue
		}
		var body io.Reader
		file, err := os.Open(f.abs)
		if err != nil {
			fail(err)
			continue
		}
		body = file
		if enc {
			if recipient == "" {
				if recipient, err = s.recipient(sphere); err != nil {
					file.Close()
					fail(err)
					continue
				}
			}
			var buf bytes.Buffer
			w, err := keys.Encrypt(&buf, recipient)
			if err == nil {
				_, err = io.Copy(w, file)
			}
			if err == nil {
				err = w.Close()
			}
			if err != nil {
				file.Close()
				fail(err)
				continue
			}
			body = &buf
		}
		h := sha256.New()
		cnt := &counter{}
		err = b.Put(ctx, remote, io.TeeReader(io.TeeReader(body, h), cnt), time.Unix(0, f.mtime))
		file.Close()
		if err != nil {
			fail(err)
			continue
		}
		if old, ok := have[f.rel]; ok && old.Remote != remote {
			_ = b.Delete(ctx, old.Remote)
		}
		rec := index.RemoteFile{Backend: b.Name, Path: f.rel, Remote: remote, SHA256: sum, RSHA256: hex.EncodeToString(h.Sum(nil)), Size: cnt.n, Synced: s.now()}
		if err := x.PutRemote(rec); err != nil {
			fail(err)
			continue
		}
		st.Uploaded++
		st.Bytes += cnt.n
	}
	for _, r := range recs {
		if seen[r.Path] {
			continue
		}
		if dry {
			st.Deleted++
			continue
		}
		if err := b.Delete(ctx, r.Remote); err != nil {
			fail(err)
			continue
		}
		if err := x.DeleteRemote(b.Name, r.Path); err != nil {
			fail(err)
			continue
		}
		st.Deleted++
	}
	if !dry && prefix != "" && len(locals) == 0 {
		if err := b.Purge(ctx, strings.TrimSuffix(prefix, "/")); err != nil {
			fail(err)
		}
	}
	return st
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func (s *Store) recipient(sphere string) (string, error) {
	if r := s.Cfg.Keys[sphere]; r != "" {
		return r, nil
	}
	id, err := s.Keys.Identity(sphere)
	if err != nil {
		return "", err
	}
	return keys.Recipient(id)
}

// SphereSync is the result of a sync of one sphere.
type SphereSync struct {
	Sphere   string      `json:"sphere"`
	Backends []SyncStats `json:"backends"`
}

// Sync brings every backend of the spheres up to date; dry counts only.
func (s *Store) Sync(ctx context.Context, sphere string, dry bool) ([]SphereSync, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	var out []SphereSync
	for _, sp := range spheres {
		bs, err := s.backendsOf(ctx, sp)
		if err != nil {
			return nil, err
		}
		res := SphereSync{Sphere: sp, Backends: []SyncStats{}}
		for _, b := range bs {
			res.Backends = append(res.Backends, s.syncPrefix(ctx, sp, b, "", dry))
		}
		out = append(out, res)
	}
	return out, nil
}

// syncObject brings one object up to date on every backend of its sphere.
// It is called right after an object is rewritten or removed, so that no
// erased version outlives the operation on a backend.
func (s *Store) syncObject(ctx context.Context, sphere, rel string) []SyncStats {
	bs, err := s.backendsOf(ctx, sphere)
	if err != nil {
		return []SyncStats{{Backend: "*", Errors: []string{err.Error()}}}
	}
	var out []SyncStats
	for _, b := range bs {
		out = append(out, s.syncPrefix(ctx, sphere, b, filepath.ToSlash(rel)+"/", false))
	}
	return out
}

// remoteLocations lists the backends that hold files of an object.
func (s *Store) remoteLocations(sphere, rel string) ([]Location, error) {
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	names, err := x.RemoteBackends(filepath.ToSlash(rel) + "/")
	if err != nil {
		return nil, err
	}
	var out []Location
	for _, n := range names {
		remote := n
		if b, ok := s.Cfg.Backends[n]; ok {
			remote = strings.TrimRight(b.Remote, "/") + "/" + sphere
		}
		out = append(out, Location{Backend: n, Path: remote + "/" + filepath.ToSlash(rel)})
	}
	return out, nil
}

// Copy is the state of an artefact on one backend.
type Copy struct {
	Backend  string `json:"backend"`
	Complete bool   `json:"complete"`
	Files    int    `json:"files"`
	Missing  int    `json:"missing"`
	Verified string `json:"verified,omitempty"`
	Trusted  bool   `json:"trusted"`
}

// copiesOf compares an object's local files with what each backend holds.
func (s *Store) copiesOf(sphere, rel string) ([]Copy, error) {
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	locals, err := s.localFiles(sphere, filepath.ToSlash(rel)+"/")
	if err != nil {
		return nil, err
	}
	var out []Copy
	for _, name := range s.Cfg.ReplicationOf(sphere).Backends {
		recs, err := x.Remotes(name, filepath.ToSlash(rel)+"/")
		if err != nil {
			return nil, err
		}
		by := map[string]index.RemoteFile{}
		for _, r := range recs {
			by[r.Path] = r
		}
		c := Copy{Backend: name, Trusted: s.Cfg.Backends[name].IsTrusted()}
		oldest := ""
		for _, f := range locals {
			r, ok := by[f.rel]
			sum, _ := s.localHash(x, f)
			if !ok || r.SHA256 != sum {
				c.Missing++
				continue
			}
			c.Files++
			if r.Verified != "" && (oldest == "" || r.Verified < oldest) {
				oldest = r.Verified
			}
		}
		c.Complete = c.Missing == 0 && len(locals) > 0
		c.Verified = oldest
		out = append(out, c)
	}
	return out, nil
}

// Coverage counts, per sphere, the artefacts below numcopies and mincopies.
type Coverage struct {
	Sphere         string `json:"sphere"`
	NumCopies      int    `json:"numcopies"`
	MinCopies      int    `json:"mincopies"`
	Artefacts      int    `json:"artefacts"`
	BelowNumCopies int    `json:"below_numcopies"`
	BelowMinCopies int    `json:"below_mincopies"`
}

func (s *Store) Coverage(sphere string) ([]Coverage, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	var out []Coverage
	for _, sp := range spheres {
		pol := s.Cfg.ReplicationOf(sp)
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		rows, err := x.List(index.Filter{})
		if err != nil {
			return nil, err
		}
		cv := Coverage{Sphere: sp, NumCopies: pol.NumCopies, MinCopies: pol.MinCopies, Artefacts: len(rows)}
		for _, r := range rows {
			cs, err := s.copiesOf(sp, r.Path)
			if err != nil {
				return nil, err
			}
			n := 1 // the local store
			for _, c := range cs {
				if c.Complete && c.Trusted {
					n++
				}
			}
			if n < pol.NumCopies {
				cv.BelowNumCopies++
			}
			if n < pol.MinCopies {
				cv.BelowMinCopies++
			}
		}
		out = append(out, cv)
	}
	return out, nil
}

// FsckResult is what a check found on one sphere.
type FsckResult struct {
	Sphere        string   `json:"sphere"`
	LocalChecked  int      `json:"local_checked"`
	LocalBad      []string `json:"local_bad"`
	RemoteChecked int      `json:"remote_checked"`
	RemoteBad     []string `json:"remote_bad"` // dropped from the record: the next sync sends them again
}

// Fsck verifies the digests of local objects and of the copies on the
// backends. sample > 0 checks that many files at random; deep downloads a
// copy when the backend gives no SHA-256.
func (s *Store) Fsck(ctx context.Context, sphere string, sample int, deep bool) ([]FsckResult, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	var out []FsckResult
	for _, sp := range spheres {
		res := FsckResult{Sphere: sp, LocalBad: []string{}, RemoteBad: []string{}}
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		rows, err := x.List(index.Filter{})
		if err != nil {
			return nil, err
		}
		pick(rows, sample)
		for _, r := range rows {
			if sample > 0 && res.LocalChecked >= sample {
				break
			}
			res.LocalChecked++
			if err := ocfl.Verify(filepath.Join(s.root(sp), r.Path)); err != nil {
				res.LocalBad = append(res.LocalBad, URI(sp, r.ID))
			}
		}
		bs, err := s.backendsOf(ctx, sp)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			recs, err := x.Remotes(b.Name, "")
			if err != nil {
				return nil, err
			}
			pick(recs, sample)
			for i, r := range recs {
				if sample > 0 && i >= sample {
					break
				}
				res.RemoteChecked++
				ok, err := s.checkRemote(ctx, b, r, deep)
				if err != nil {
					return nil, err
				}
				if !ok {
					res.RemoteBad = append(res.RemoteBad, b.Name+":"+r.Remote)
					_ = x.DeleteRemote(b.Name, r.Path)
					continue
				}
				_ = x.SetVerified(b.Name, r.Path, s.now())
			}
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Store) checkRemote(ctx context.Context, b *backend.Backend, r index.RemoteFile, deep bool) (bool, error) {
	in, err := b.Stat(ctx, r.Remote)
	if errors.Is(err, backend.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if in.Size != r.Size {
		return false, nil
	}
	if in.SHA256 != "" {
		return strings.EqualFold(in.SHA256, r.RSHA256), nil
	}
	if !deep {
		return true, nil
	}
	rc, err := b.Open(ctx, r.Remote)
	if err != nil {
		return false, nil
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return false, nil
	}
	return hex.EncodeToString(h.Sum(nil)) == r.RSHA256, nil
}

func pick[T any](l []T, sample int) {
	if sample > 0 && sample < len(l) {
		rand.Shuffle(len(l), func(i, j int) { l[i], l[j] = l[j], l[i] })
	}
}

// BackendFiles lists what a backend holds for the sphere, for tests and audits.
func (s *Store) BackendFiles(ctx context.Context, sphere, name string) ([]string, error) {
	bs, err := s.backendsOf(ctx, sphere)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		if b.Name == name {
			l, err := b.List(ctx, "")
			sort.Strings(l)
			return l, err
		}
	}
	return nil, fmt.Errorf("backend %q is not a backend of sphere %q", name, sphere)
}
