// Package view presents one sphere's artefacts as a read-only file tree
// (SPEC §11.1), served over NFS and WebDAV. Files are the store's own
// content files, opened read-only; sealed artefacts never appear.
package view

import (
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// Tree of one sphere:
//
//	a/<ulid><ext>             current version
//	a/<ulid>/v<N><ext>        fixed versions
type Tree struct {
	st     *store.Store
	sphere string
	ttl    time.Duration

	mu      sync.Mutex
	rows    map[string]index.Row // by id
	order   []string
	fetched time.Time
	started time.Time
}

func New(st *store.Store, sphere string) *Tree {
	return &Tree{st: st, sphere: sphere, ttl: 2 * time.Second, started: time.Now()}
}

func (t *Tree) Sphere() string { return t.sphere }

func (t *Tree) snapshot() (map[string]index.Row, []string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rows != nil && time.Since(t.fetched) < t.ttl {
		return t.rows, t.order, nil
	}
	rows, err := t.st.ViewRows(t.sphere)
	if err != nil {
		return nil, nil, err
	}
	m := make(map[string]index.Row, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		m[r.ID] = r
		order = append(order, r.ID)
	}
	sort.Strings(order)
	t.rows, t.order, t.fetched = m, order, time.Now()
	return m, order, nil
}

// Info describes one node of the tree.
type Info struct {
	name string
	size int64
	mod  time.Time
	dir  bool
	real string // content file, for a file
}

func (i *Info) Name() string { return i.name }
func (i *Info) Size() int64  { return i.size }
func (i *Info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i *Info) ModTime() time.Time { return i.mod }
func (i *Info) IsDir() bool        { return i.dir }
func (i *Info) Sys() any           { return nil }

// Real is the content file behind a file node.
func (i *Info) Real() string { return i.real }

func (t *Tree) dir(name string) *Info {
	return &Info{name: name, dir: true, mod: t.started}
}

func fileInfo(name, real string) (*Info, error) {
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	return &Info{name: name, size: fi.Size(), mod: fi.ModTime(), real: real}, nil
}

func clean(p string) []string {
	p = path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
	if p == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// Stat resolves a path of the tree.
func (t *Tree) Stat(p string) (*Info, error) {
	parts := clean(p)
	switch {
	case len(parts) == 0:
		return t.dir("/"), nil
	case parts[0] != "a":
		return nil, fs.ErrNotExist
	case len(parts) == 1:
		return t.dir("a"), nil
	}
	rows, _, err := t.snapshot()
	if err != nil {
		return nil, err
	}
	name := parts[1]
	if len(parts) == 2 {
		if r, ok := rows[name]; ok {
			return t.dir(r.ID), nil
		}
		id := strings.TrimSuffix(name, path.Ext(name))
		r, ok := rows[id]
		if !ok || r.ID+r.Ext != name {
			return nil, fs.ErrNotExist
		}
		vs, err := t.st.VersionFiles(t.sphere, id)
		if err != nil || len(vs) == 0 {
			return nil, fs.ErrNotExist
		}
		// Same size and time as the listing of a/, so clients see one file.
		return &Info{name: name, size: r.Size, mod: parseTime(r.Updated, t.started), real: vs[len(vs)-1].Path}, nil
	}
	if len(parts) == 3 {
		r, ok := rows[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		vs, err := t.st.VersionFiles(t.sphere, r.ID)
		if err != nil {
			return nil, fs.ErrNotExist
		}
		for _, v := range vs {
			if v.Version+r.Ext == parts[2] {
				return fileInfo(parts[2], v.Path)
			}
		}
	}
	return nil, fs.ErrNotExist
}

// ReadDir lists a directory of the tree.
func (t *Tree) ReadDir(p string) ([]*Info, error) {
	parts := clean(p)
	switch {
	case len(parts) == 0:
		return []*Info{t.dir("a")}, nil
	case parts[0] != "a" || len(parts) > 2:
		return nil, fs.ErrNotExist
	}
	rows, order, err := t.snapshot()
	if err != nil {
		return nil, err
	}
	if len(parts) == 1 {
		out := make([]*Info, 0, 2*len(order))
		for _, id := range order {
			r := rows[id]
			out = append(out, &Info{name: id + r.Ext, size: r.Size, mod: parseTime(r.Updated, t.started)}, t.dir(id))
		}
		return out, nil
	}
	r, ok := rows[parts[1]]
	if !ok {
		return nil, fs.ErrNotExist
	}
	vs, err := t.st.VersionFiles(t.sphere, r.ID)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	var out []*Info
	for _, v := range vs {
		if fi, err := fileInfo(v.Version+r.Ext, v.Path); err == nil {
			out = append(out, fi)
		}
	}
	return out, nil
}

func parseTime(s string, def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

// Open opens a file node read-only.
func (t *Tree) Open(p string) (*os.File, *Info, error) {
	fi, err := t.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	if fi.dir {
		return nil, fi, nil
	}
	f, err := os.Open(fi.real)
	return f, fi, err
}
