// Package store implements the actions on artefacts: one OCFL storage root
// and one index per sphere (SPEC §4).
package store

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

// UserError is an error the caller can fix; actions map it to user_error.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{fmt.Sprintf(format, a...)} }

// NotFound reports an unknown artefact.
type NotFound struct{ Msg string }

func (e *NotFound) Error() string { return e.Msg }

type Store struct {
	Cfg     *config.Config
	Spheres []string // spheres granted to the caller
	Agent   string   // who acts, written in events
	Now     func() time.Time

	indexes map[string]*index.Index
}

// Open opens the store for the granted spheres (all configured ones when empty).
func Open(cfg *config.Config, spheres []string, agent string) (*Store, error) {
	if len(spheres) == 0 {
		spheres = cfg.Spheres
	}
	for _, s := range spheres {
		if !slices.Contains(cfg.Spheres, s) {
			return nil, userErr("sphere %q is not configured; configured spheres: %s", s, strings.Join(cfg.Spheres, ", "))
		}
	}
	if agent == "" {
		agent = "artefact"
	}
	return &Store{Cfg: cfg, Spheres: spheres, Agent: agent, Now: time.Now, indexes: map[string]*index.Index{}}, nil
}

func (s *Store) Close() {
	for _, x := range s.indexes {
		x.Close()
	}
}

func (s *Store) root(sphere string) string { return filepath.Join(s.Cfg.StorePath(), sphere) }
func (s *Store) state() string             { return filepath.Join(s.Cfg.StorePath(), ".artefact") }
func URI(sphere, id string) string         { return "artefact://" + sphere + "/" + id }
func (s *Store) now() string               { return s.Now().Format(time.RFC3339) }

// Init creates the storage roots and indexes of every configured sphere.
func (s *Store) Init() error {
	if err := os.MkdirAll(s.state(), 0o755); err != nil {
		return err
	}
	for _, sp := range s.Cfg.Spheres {
		if err := ocfl.InitRoot(s.root(sp)); err != nil {
			return err
		}
		if _, err := s.index(sp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) index(sphere string) (*index.Index, error) {
	if x, ok := s.indexes[sphere]; ok {
		return x, nil
	}
	if !ocfl.IsRoot(s.root(sphere)) {
		return nil, fmt.Errorf("no store for sphere %q at %s. Run `artefact init`", sphere, s.root(sphere))
	}
	if err := os.MkdirAll(s.state(), 0o755); err != nil {
		return nil, err
	}
	x, err := index.Open(filepath.Join(s.state(), "index-"+sphere+".sqlite"))
	if err != nil {
		return nil, err
	}
	s.indexes[sphere] = x
	return x, nil
}

// lock takes the sphere's write lock until the returned function is called.
func (s *Store) lock(sphere string) (func(), error) {
	if err := os.MkdirAll(s.state(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.state(), "lock-"+sphere), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (s *Store) sphereOrDefault(sphere string) (string, error) {
	if sphere == "" {
		sphere = s.Cfg.DefaultSphere
		if !slices.Contains(s.Spheres, sphere) {
			sphere = s.Spheres[0]
		}
	}
	if !slices.Contains(s.Spheres, sphere) {
		return "", userErr("sphere %q is not granted here; granted spheres: %s. Example: --sphere %s", sphere, strings.Join(s.Spheres, ", "), s.Spheres[0])
	}
	return sphere, nil
}

// Ref is a resolved artefact.
type Ref struct {
	Sphere string
	ID     string
	Row    index.Row
	Dir    string // object root, absolute
}

// Resolve finds an artefact by ULID or URI among the granted spheres.
func (s *Store) Resolve(ref string) (*Ref, error) {
	sphere, id := "", ref
	if strings.HasPrefix(ref, "artefact://") {
		rest := strings.TrimPrefix(ref, "artefact://")
		var ok bool
		sphere, id, ok = strings.Cut(rest, "/")
		if !ok || sphere == "" || id == "" {
			return nil, userErr("%q is not an artefact URI. Example: artefact://perso/01K9B3T0Z6Q8M2C4R7V1X5N9PA", ref)
		}
		if !slices.Contains(s.Spheres, sphere) {
			return nil, userErr("sphere %q is not granted here; granted spheres: %s", sphere, strings.Join(s.Spheres, ", "))
		}
	}
	if _, err := ulid.ParseStrict(id); err != nil {
		return nil, userErr("%q is not an artefact id: a 26-character ULID or an artefact:// URI. Example: artefact stat 01K9B3T0Z6Q8M2C4R7V1X5N9PA", ref)
	}
	spheres := s.Spheres
	if sphere != "" {
		spheres = []string{sphere}
	}
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		row, err := x.Get(id)
		if errors.Is(err, index.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &Ref{Sphere: sp, ID: id, Row: row, Dir: filepath.Join(s.root(sp), row.Path)}, nil
	}
	return nil, &NotFound{fmt.Sprintf("no artefact %s in the granted spheres (%s)", id, strings.Join(spheres, ", "))}
}

// PutOptions describes a deposit.
type PutOptions struct {
	Sphere   string
	Filename string
	Title    string
	Date     string
	Mime     string
	Holds    []index.Holder // By and Ref; At is set by Put
	Card     string         // YAML of editable fields, merged into the card
	Key      string         // idempotency key, scoped to the sphere
}

type PutResult struct {
	ID      string         `json:"id"`
	URI     string         `json:"uri"`
	Sphere  string         `json:"sphere"`
	Version int            `json:"version"`
	Status  string         `json:"status"` // created | known
	Digest  string         `json:"digest"`
	Size    int64          `json:"size"`
	Title   string         `json:"title"`
	Holders []index.Holder `json:"holders"`
}

// Put deposits r. It is atomic, and idempotent within the sphere by digest,
// or by Key when given.
func (s *Store) Put(r io.Reader, o PutOptions) (*PutResult, error) {
	sphere, err := s.sphereOrDefault(o.Sphere)
	if err != nil {
		return nil, err
	}
	if o.Date != "" && !dateRe.MatchString(o.Date) {
		return nil, userErr("--date takes YYYY-MM-DD, got %q. Example: artefact put scan.pdf --date 2026-09-18", o.Date)
	}
	for _, h := range o.Holds {
		if h.By == "" {
			return nil, userErr("--hold takes <by>:<ref>, e.g. --hold mnemo:01K6Z8Q4V7XW3D9JH2M5N8P0QR")
		}
	}
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	root := s.root(sphere)
	id := ulid.Make().String()
	now := s.Now()
	ext := strings.ToLower(filepath.Ext(o.Filename))
	user := &ocfl.User{Name: s.Agent, Address: "urn:x-artefact-agent:" + url.PathEscape(s.Agent)}
	staged, err := ocfl.Stage(root, URI(sphere, id), "payload"+ext, r, now, "deposit", user)
	if err != nil {
		return nil, err
	}
	digest := "sha256:" + staged.SHA256

	unlock, err := s.lock(sphere)
	if err != nil {
		staged.Abort()
		return nil, err
	}
	defer unlock()

	if o.Key != "" {
		kid, kdigest, err := x.Key(o.Key)
		switch {
		case err == nil && kdigest != digest:
			staged.Abort()
			return nil, userErr("key %q was first used for another payload (artefact %s); a key names one payload. Use a new key or omit --key", o.Key, kid)
		case err == nil:
			staged.Abort()
			return s.known(sphere, kid, o.Holds)
		case !errors.Is(err, index.ErrNotFound):
			staged.Abort()
			return nil, err
		}
	}
	if row, err := x.ByDigest(digest); err == nil {
		staged.Abort()
		if o.Key != "" {
			if err := x.PutKey(o.Key, row.ID, digest); err != nil {
				return nil, err
			}
		}
		return s.known(sphere, row.ID, o.Holds)
	} else if !errors.Is(err, index.ErrNotFound) {
		staged.Abort()
		return nil, err
	}

	mt := o.Mime
	if mt == "" {
		mt = detectMime(filepath.Join(staged.Dir, "v1", "content", "payload"+ext), ext)
	}
	title := o.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(o.Filename), filepath.Ext(o.Filename))
	}
	if title == "" || title == "." {
		title = id
	}
	date := o.Date
	if date == "" {
		date = now.Format("2006-01-02")
	}
	at := now.Format(time.RFC3339)
	c := &Card{
		ID: id, Sphere: sphere, Title: title, Date: date, Mime: mt, Size: staged.Size,
		Filename: filepath.Base(o.Filename), Version: 1, Digest: digest,
		Deposited: at, Updated: at, Holders: []index.Holder{},
	}
	if c.Filename == "." {
		c.Filename = ""
	}
	if o.Card != "" {
		if err := c.merge(o.Card); err != nil {
			staged.Abort()
			return nil, &UserError{err.Error()}
		}
	}
	for _, h := range o.Holds {
		h.At = at
		c.addHolder(h)
	}
	detail := map[string]any{}
	if o.Key != "" {
		detail["key"] = o.Key
	}
	if len(detail) == 0 {
		detail = nil
	}
	if err := writeCard(staged.Dir, c); err != nil {
		staged.Abort()
		return nil, err
	}
	if err := appendEvent(staged.Dir, Event{At: at, Type: "deposit", Agent: s.Agent, Outcome: "ok", Version: 1, Detail: detail}); err != nil {
		staged.Abort()
		return nil, err
	}
	for _, h := range c.Holders {
		if err := appendEvent(staged.Dir, Event{At: at, Type: "hold", Agent: s.Agent, Outcome: "ok", Detail: map[string]any{"by": h.By, "ref": h.Ref}}); err != nil {
			staged.Abort()
			return nil, err
		}
	}
	dir, err := staged.Commit(root)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, dir)
	if err := x.Put(rowOf(c, rel), c.Holders); err != nil {
		return nil, err
	}
	if o.Key != "" {
		if err := x.PutKey(o.Key, id, digest); err != nil {
			return nil, err
		}
	}
	return &PutResult{ID: id, URI: URI(sphere, id), Sphere: sphere, Version: 1, Status: "created",
		Digest: digest, Size: c.Size, Title: c.Title, Holders: c.Holders}, nil
}

// known answers a deposit of a payload already stored: it adds the holders
// and returns every holder of the artefact. The caller holds the lock.
func (s *Store) known(sphere, id string, holds []index.Holder) (*PutResult, error) {
	ref, err := s.Resolve(URI(sphere, id))
	if err != nil {
		return nil, err
	}
	c, err := readCard(ref.Dir)
	if err != nil {
		return nil, err
	}
	if len(holds) > 0 {
		if err := s.addHolders(ref, c, holds); err != nil {
			return nil, err
		}
	}
	return &PutResult{ID: id, URI: URI(sphere, id), Sphere: sphere, Version: c.Version, Status: "known",
		Digest: c.Digest, Size: c.Size, Title: c.Title, Holders: c.Holders}, nil
}

func (s *Store) addHolders(ref *Ref, c *Card, holds []index.Holder) error {
	at := s.now()
	changed := false
	for _, h := range holds {
		h.At = at
		if c.addHolder(h) {
			changed = true
			if err := appendEvent(ref.Dir, Event{At: at, Type: "hold", Agent: s.Agent, Outcome: "ok", Detail: map[string]any{"by": h.By, "ref": h.Ref}}); err != nil {
				return err
			}
		}
	}
	if !changed {
		return nil
	}
	return s.saveCard(ref, c, at)
}

func (s *Store) saveCard(ref *Ref, c *Card, at string) error {
	c.Updated = at
	if err := writeCard(ref.Dir, c); err != nil {
		return err
	}
	x, err := s.index(ref.Sphere)
	if err != nil {
		return err
	}
	return x.Put(rowOf(c, ref.Row.Path), c.Holders)
}

func rowOf(c *Card, rel string) index.Row {
	return index.Row{ID: c.ID, Digest: c.Digest, Title: c.Title, Filename: c.Filename, Mime: c.Mime, Size: c.Size,
		Date: c.Date, Version: c.Version, Sealed: c.Sealed, Deposited: c.Deposited, Updated: c.Updated, Path: rel}
}

func detectMime(path, ext string) string {
	if ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			t, _, _ = strings.Cut(t, ";")
			return t
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	t, _, _ := strings.Cut(http.DetectContentType(buf[:n]), ";")
	return t
}

// Hold records holders on an artefact. Holding twice is a no-op.
func (s *Store) Hold(ref string, by, holdRef string) (*Card, error) {
	if by == "" {
		return nil, userErr("--by names the holding tool. Example: artefact hold 01K9B3T0Z6Q8M2C4R7V1X5N9PA --by mnemo --ref 01K6Z8Q4V7XW3D9JH2M5N8P0QR")
	}
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
	if err := s.addHolders(r, c, []index.Holder{{By: by, Ref: holdRef}}); err != nil {
		return nil, err
	}
	return c, nil
}

// Release removes the holders of by (only ref when given). Releasing an
// absent holder succeeds.
func (s *Store) Release(ref string, by, holdRef string) (*Card, int, error) {
	if by == "" {
		return nil, 0, userErr("--by names the holding tool. Example: artefact release 01K9B3T0Z6Q8M2C4R7V1X5N9PA --by mnemo --ref 01K6Z8Q4V7XW3D9JH2M5N8P0QR")
	}
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, 0, err
	}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, 0, err
	}
	n := c.removeHolders(by, holdRef)
	if n == 0 {
		return c, 0, nil
	}
	at := s.now()
	if err := appendEvent(r.Dir, Event{At: at, Type: "release", Agent: s.Agent, Outcome: "ok", Detail: map[string]any{"by": by, "ref": holdRef, "count": n}}); err != nil {
		return nil, 0, err
	}
	return c, n, s.saveCard(r, c, at)
}

// EditCard applies --set fields and a --merge document.
func (s *Store) EditCard(ref string, sets []string, mergeDoc string) (*Card, error) {
	if len(sets) == 0 && mergeDoc == "" {
		return nil, userErr("nothing to change: pass --set <field>=<value> or --merge <yaml>. Example: artefact card 01K9B3T0Z6Q8M2C4R7V1X5N9PA --set title=\"Ordonnance septembre 2026\"")
	}
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
	var fields []string
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, userErr("--set takes <field>=<value>, got %q. Example: --set date=2026-09-18", kv)
		}
		if err := c.set(k, v); err != nil {
			return nil, &UserError{err.Error()}
		}
		fields = append(fields, k)
	}
	if mergeDoc != "" {
		if err := c.merge(mergeDoc); err != nil {
			return nil, &UserError{err.Error()}
		}
		fields = append(fields, "merge")
	}
	at := s.now()
	if err := appendEvent(r.Dir, Event{At: at, Type: "card", Agent: s.Agent, Outcome: "ok", Detail: map[string]any{"fields": fields}}); err != nil {
		return nil, err
	}
	return c, s.saveCard(r, c, at)
}

// Open returns the payload of version v ("" for the current one).
func (s *Store) OpenPayload(ref, v string) (io.ReadCloser, *Card, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, nil, err
	}
	inv, err := ocfl.ReadInventory(r.Dir)
	if err != nil {
		return nil, nil, err
	}
	files, err := inv.Files(v)
	if err != nil {
		return nil, nil, &UserError{err.Error()}
	}
	for _, f := range files {
		if strings.HasPrefix(f.Logical, "payload") {
			rc, err := os.Open(filepath.Join(r.Dir, filepath.FromSlash(f.Content)))
			return rc, c, err
		}
	}
	return nil, nil, fmt.Errorf("artefact %s has no payload in version %s", r.ID, v)
}

type VersionInfo struct {
	Version string `json:"version"`
	Created string `json:"created"`
	Digest  string `json:"digest"`
	Message string `json:"message,omitempty"`
}

type Location struct {
	Backend string `json:"backend"`
	Path    string `json:"path"`
}

type Stat struct {
	URI       string        `json:"uri"`
	Card      *Card         `json:"card"`
	Versions  []VersionInfo `json:"versions"`
	Locations []Location    `json:"locations"`
	Events    []Event       `json:"events,omitempty"`
}

// Stat describes artefacts; unknown refs are listed apart.
func (s *Store) Stat(refs []string, withEvents bool) ([]Stat, []string, error) {
	out := []Stat{}
	missing := []string{}
	for _, ref := range refs {
		r, err := s.Resolve(ref)
		var nf *NotFound
		if errors.As(err, &nf) {
			missing = append(missing, ref)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		c, err := readCard(r.Dir)
		if err != nil {
			return nil, nil, err
		}
		inv, err := ocfl.ReadInventory(r.Dir)
		if err != nil {
			return nil, nil, err
		}
		st := Stat{URI: URI(r.Sphere, r.ID), Card: c, Locations: []Location{{Backend: "local", Path: r.Dir}}}
		for _, vn := range inv.VersionNames() {
			vi := VersionInfo{Version: vn, Created: inv.Versions[vn].Created, Message: inv.Versions[vn].Message}
			if files, err := inv.Files(vn); err == nil {
				for _, f := range files {
					if strings.HasPrefix(f.Logical, "payload") {
						vi.Digest = "sha256:" + f.SHA256
					}
				}
			}
			st.Versions = append(st.Versions, vi)
		}
		if withEvents {
			if st.Events, err = readEvents(r.Dir); err != nil {
				return nil, nil, err
			}
		}
		out = append(out, st)
	}
	return out, missing, nil
}

type ListItem struct {
	index.Row
	Sphere string `json:"sphere"`
	URI    string `json:"uri"`
}

// List lists artefacts of one sphere, or of every granted one.
func (s *Store) List(sphere string, f index.Filter) ([]ListItem, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	out := []ListItem{}
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		rows, err := x.List(f)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, ListItem{Row: r, Sphere: sp, URI: URI(sp, r.ID)})
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

type SphereUsage struct {
	Sphere string `json:"sphere"`
	index.Usage
	DiskBytes int64 `json:"disk_bytes"`
}

// Usage counts artefacts and bytes per sphere.
func (s *Store) Usage(sphere string) ([]SphereUsage, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	var out []SphereUsage
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		u, err := x.Usage()
		if err != nil {
			return nil, err
		}
		var disk int64
		_ = filepath.Walk(s.root(sp), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				disk += fi.Size()
			}
			return nil
		})
		out = append(out, SphereUsage{Sphere: sp, Usage: u, DiskBytes: disk})
	}
	return out, nil
}

type ReindexResult struct {
	Sphere    string `json:"sphere"`
	Artefacts int    `json:"artefacts"`
}

// Reindex rebuilds the indexes from the store: cards, inventories, events.
func (s *Store) Reindex(sphere string) ([]ReindexResult, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	var out []ReindexResult
	for _, sp := range spheres {
		res, err := s.reindex(sp)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Store) reindex(sphere string) (ReindexResult, error) {
	x, err := s.index(sphere)
	if err != nil {
		return ReindexResult{}, err
	}
	unlock, err := s.lock(sphere)
	if err != nil {
		return ReindexResult{}, err
	}
	defer unlock()
	if err := x.Reset(); err != nil {
		return ReindexResult{}, err
	}
	root := s.root(sphere)
	n := 0
	err = ocfl.Walk(root, func(dir string) error {
		c, err := readCard(dir)
		if err != nil {
			return err
		}
		if c.Sphere != sphere {
			return fmt.Errorf("object %s carries sphere %q in the store of %q", dir, c.Sphere, sphere)
		}
		rel, _ := filepath.Rel(root, dir)
		if err := x.Put(rowOf(c, rel), c.Holders); err != nil {
			return err
		}
		evs, err := readEvents(dir)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if k, ok := e.Detail["key"].(string); ok && e.Type == "deposit" {
				if err := x.PutKey(k, c.ID, c.Digest); err != nil {
					return err
				}
			}
		}
		n++
		return nil
	})
	return ReindexResult{Sphere: sphere, Artefacts: n}, err
}
