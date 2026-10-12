// Package store implements the actions on artefacts: one OCFL storage root
// and one index per sphere (SPEC §4).
package store

import (
	"errors"
	"fmt"
	"github.com/aclemen1/artefact-cli/internal/notes"
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

	"github.com/aclemen1/jev"
	"github.com/oklog/ulid/v2"

	"github.com/aclemen1/artefact-cli/internal/backend"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/meta"
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
	Agent   string   // the software or channel that acts, written in events
	By      string   // the actor who signs the writes (SPEC §12.1), written in events and versions
	Now     func() time.Time
	Keys    keys.Source
	Jev     *jev.Client // opened on the first jev or openjev decision; released by Close

	indexes  map[string]*index.Index
	backends map[string]*backend.Backend
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
	return &Store{Cfg: cfg, Spheres: spheres, Agent: agent, Now: time.Now, Keys: keys.Keychain{}, indexes: map[string]*index.Index{}}, nil
}

func (s *Store) Close() {
	for _, x := range s.indexes {
		x.Close()
	}
	if s.Jev != nil {
		s.Jev.Close()
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
	release := func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
	if err := ocfl.Recover(s.root(sphere)); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// sphereOrDefault checks that sphere is named and granted; there is no
// default sphere: a write names the sphere its content belongs to.
func (s *Store) sphereOrDefault(sphere string) (string, error) {
	if sphere == "" {
		return "", userErr("name the sphere: --sphere <%s>", strings.Join(s.Spheres, "|"))
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
	Seal     bool           // encrypt with the sphere's age key before anything is written
}

type PutResult struct {
	ID      string         `json:"id"`
	URI     string         `json:"uri"`
	Sphere  string         `json:"sphere"`
	Version int            `json:"version"`
	Status  string         `json:"status"` // created | known
	Digest  string         `json:"digest"`
	Size    int64          `json:"size"`
	Sealed  bool           `json:"sealed"`
	Title   string         `json:"title"`
	Holders []index.Holder `json:"holders"`
	Render  *RenderResult  `json:"render,omitempty"`
}

// sealing encrypts what is read from r for recipient, computing on the way
// the keyed digest and the size of the plaintext.
type sealing struct {
	r     *io.PipeReader
	done  chan struct{}
	keyed func() string
	size  int64
}

func (s *Store) seal(sphere string, r io.Reader) (*sealing, error) {
	identity, err := s.Keys.Identity(sphere)
	if err != nil {
		return nil, userErr("%v", err)
	}
	recipient := s.Cfg.Keys[sphere]
	if recipient == "" {
		// A throwaway store may give only the identity, through the environment.
		if recipient, err = keys.Recipient(identity); err != nil {
			return nil, userErr("sphere %q has no age key. Run `artefact key init --sphere %s` first", sphere, sphere)
		}
	}
	pr, pw := io.Pipe()
	kw, keyed := keys.Keyed(keys.DedupKey(identity))
	sl := &sealing{r: pr, done: make(chan struct{}), keyed: keyed}
	go func() {
		defer close(sl.done)
		// age writes its header at once: the encryptor lives on this side
		// of the pipe so that the reader is already there.
		enc, err := keys.Encrypt(pw, recipient)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		n, err := io.Copy(enc, io.TeeReader(r, kw))
		sl.size = n
		if err == nil {
			err = enc.Close()
		}
		pw.CloseWithError(err)
	}()
	return sl, nil
}

// Put deposits r. It is atomic, and idempotent within the sphere by digest
// (keyed digest for a sealed payload), or by Key when given.
func (s *Store) Put(r io.Reader, o PutOptions) (*PutResult, error) {
	if o.Sphere == "" {
		return nil, userErr("put needs --sphere: the sphere follows the content (a UNISIS document goes to pro), never a default. Example: artefact put scan.pdf --sphere pro")
	}
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
	logical := "payload" + ext
	var sl *sealing
	if o.Seal {
		if sl, err = s.seal(sphere, r); err != nil {
			return nil, err
		}
		r = sl.r
		logical += ".age"
	}
	staged, err := ocfl.Stage(root, URI(sphere, id), logical, r, now, "deposit", s.user())
	if sl != nil {
		sl.r.CloseWithError(err) // unblocks the encryptor if staging stopped early
		<-sl.done
	}
	if err != nil {
		return nil, err
	}
	digest := "sha256:" + staged.SHA256
	size, match := staged.Size, digest
	keyed := ""
	if sl != nil {
		size, keyed = sl.size, sl.keyed()
		match = keyed
	}

	unlock, err := s.lock(sphere)
	if err != nil {
		staged.Abort()
		return nil, err
	}
	defer unlock()

	if o.Key != "" {
		kid, kmatch, err := x.Key(o.Key)
		switch {
		case err == nil && kmatch != match:
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
	var row index.Row
	if sl != nil {
		row, err = x.ByKeyed(keyed)
	} else {
		row, err = x.ByDigest(digest)
	}
	if err == nil {
		staged.Abort()
		if o.Key != "" {
			if err := x.PutKey(o.Key, row.ID, match); err != nil {
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
		if sl != nil {
			mt = detectMime("", ext)
		} else {
			mt = detectMime(filepath.Join(staged.Dir, "v1", "content", logical), ext)
		}
	}
	title := o.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(o.Filename), filepath.Ext(o.Filename))
	}
	if title == "" || title == "." {
		title = id
	}
	var emb *meta.Embedded
	if sl == nil && meta.Available() {
		if e, err := meta.Read(filepath.Join(staged.Dir, "v1", "content", logical)); err == nil && !e.Empty() {
			emb = &e
		}
	}
	date := o.Date
	if date == "" && emb != nil && emb.Date != "" {
		date = emb.Date // the content's own date: taken, created
	}
	if date == "" {
		date = now.Format("2006-01-02")
	}
	at := now.Format(time.RFC3339)
	c := &Card{
		ID: id, Sphere: sphere, Title: title, Date: date, Mime: mt, Size: size,
		Filename: filepath.Base(o.Filename), Ext: ext, Version: 1, Digest: digest, Sealed: sl != nil, Keyed: keyed, Embedded: emb,
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
	if c.Filename != "" {
		detail["filename"] = c.Filename
	}
	if len(detail) == 0 {
		detail = nil
	}
	if err := writeCard(staged.Dir, c); err != nil {
		staged.Abort()
		return nil, err
	}
	if err := appendEvent(staged.Dir, Event{At: at, Type: "deposit", Agent: s.Agent, By: s.By, Outcome: "ok", Version: 1, Detail: detail}); err != nil {
		staged.Abort()
		return nil, err
	}
	for _, h := range c.Holders {
		if err := appendEvent(staged.Dir, Event{At: at, Type: "hold", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"by": h.By, "ref": h.Ref}}); err != nil {
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
	if err := s.indexSearch(x, dir, c); err != nil {
		return nil, err
	}
	if o.Key != "" {
		if err := x.PutKey(o.Key, id, match); err != nil {
			return nil, err
		}
	}
	return &PutResult{ID: id, URI: URI(sphere, id), Sphere: sphere, Version: 1, Status: "created",
		Digest: digest, Size: c.Size, Sealed: c.Sealed, Title: c.Title, Holders: c.Holders}, nil
}

func (s *Store) user() *ocfl.User {
	if s.By != "" {
		return &ocfl.User{Name: s.By, Address: "urn:x-artefact-actor:" + url.PathEscape(s.By)}
	}
	return &ocfl.User{Name: s.Agent, Address: "urn:x-artefact-agent:" + url.PathEscape(s.Agent)}
}

// matchDigest is what deduplication and idempotency keys compare.
func (c *Card) matchDigest() string {
	if c.Sealed {
		return c.Keyed
	}
	return c.Digest
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
		Digest: c.Digest, Size: c.Size, Sealed: c.Sealed, Title: c.Title, Holders: c.Holders}, nil
}

func (s *Store) addHolders(ref *Ref, c *Card, holds []index.Holder) error {
	at := s.now()
	changed := false
	for _, h := range holds {
		h.At = at
		if c.addHolder(h) {
			changed = true
			if err := appendEvent(ref.Dir, Event{At: at, Type: "hold", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"by": h.By, "ref": h.Ref}}); err != nil {
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
	if err := x.Put(rowOf(c, ref.Row.Path), c.Holders); err != nil {
		return err
	}
	if err := x.PutClasses(c.ID, classRows(c)); err != nil {
		return err
	}
	return s.indexSearch(x, ref.Dir, c)
}

func classRows(c *Card) []index.ClassRow {
	var out []index.ClassRow
	for plan, k := range c.Classes {
		out = append(out, index.ClassRow{Plan: plan, Node: k.Node, By: k.By, Confidence: k.Confidence, Pending: k.Pending})
	}
	return out
}

func rowOf(c *Card, rel string) index.Row {
	return index.Row{ID: c.ID, Digest: c.Digest, Title: c.Title, Filename: c.Filename, Mime: c.Mime, Size: c.Size,
		Date: c.Date, Version: c.Version, Sealed: c.Sealed, Deposited: c.Deposited, Updated: c.Updated, Path: rel, Keyed: c.Keyed, Ext: c.Ext, Archived: c.Archived}
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
	if err := appendEvent(r.Dir, Event{At: at, Type: "release", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"by": by, "ref": holdRef, "count": n}}); err != nil {
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
	if err := appendEvent(r.Dir, Event{At: at, Type: "card", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"fields": fields}}); err != nil {
		return nil, err
	}
	return c, s.saveCard(r, c, at)
}

// Open returns the payload of version v ("" for the current one).
func (s *Store) OpenPayload(ref, v string, reveal bool) (io.ReadCloser, *Card, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, nil, err
	}
	if c.Sealed && !reveal {
		return nil, nil, userErr("artefact %s is sealed; pass --reveal to decrypt it. Example: artefact get %s --reveal --out /tmp/task/file", c.ID, c.ID)
	}
	f, err := payloadFile(r.Dir, v)
	if err != nil {
		return nil, nil, err
	}
	rc, err := os.Open(f)
	if err != nil || !c.Sealed {
		return rc, c, err
	}
	identity, err := s.Keys.Identity(r.Sphere)
	if err != nil {
		rc.Close()
		return nil, nil, userErr("%v", err)
	}
	plain, err := keys.Decrypt(rc, identity)
	if err != nil {
		rc.Close()
		return nil, nil, err
	}
	return readCloser{plain, rc}, c, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// payloadFile is the content path of the payload of version v ("" for the head).
func payloadFile(objDir, v string) (string, error) {
	inv, err := ocfl.ReadInventory(objDir)
	if err != nil {
		return "", err
	}
	files, err := inv.Files(v)
	if err != nil {
		return "", &UserError{err.Error()}
	}
	for _, f := range files {
		if strings.HasPrefix(f.Logical, "payload") {
			return ocfl.ContentPath(objDir, f), nil
		}
	}
	return "", fmt.Errorf("object %s has no payload in version %q", inv.ID, v)
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
	Copies    []Copy        `json:"copies,omitempty"`
	Events    []Event       `json:"events,omitempty"`
	Notes     []notes.Entry `json:"notes,omitempty"`
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
		if st.Copies, err = s.copiesOf(r.Sphere, r.Row.Path); err != nil {
			return nil, nil, err
		}
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
	if f.SortKey == "relevance" {
		return nil, userErr("relevance applies to a search only; ls sorts by %s", strings.Join(SortKeys[1:], ", "))
	}
	sortItems(out, Sort{Key: f.SortKey, Desc: f.SortDesc}.orDefault("deposited"))
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
		if c.Ext == "" {
			c.Ext = payloadExt(dir)
		}
		rel, _ := filepath.Rel(root, dir)
		if err := x.Put(rowOf(c, rel), c.Holders); err != nil {
			return err
		}
		if err := x.PutClasses(c.ID, classRows(c)); err != nil {
			return err
		}
		if err := s.indexSearch(x, dir, c); err != nil {
			return err
		}
		evs, err := readEvents(dir)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if k, ok := e.Detail["key"].(string); ok && e.Type == "deposit" {
				if err := x.PutKey(k, c.ID, c.matchDigest()); err != nil {
					return err
				}
			}
		}
		n++
		return nil
	})
	return ReindexResult{Sphere: sphere, Artefacts: n}, err
}

// FindDigest returns the id of the unsealed artefact of sphere whose current
// payload has this digest (sha256:<hex>).
func (s *Store) FindDigest(sphere, digest string) (string, error) {
	sp, err := s.sphereOrDefault(sphere)
	if err != nil {
		return "", err
	}
	x, err := s.index(sp)
	if err != nil {
		return "", err
	}
	row, err := x.ByDigest(digest)
	if errors.Is(err, index.ErrNotFound) {
		return "", &NotFound{fmt.Sprintf("no artefact with digest %s in sphere %s", digest, sp)}
	}
	return row.ID, err
}

// ViewRows lists the unsealed artefacts of sphere, for the file views.
func (s *Store) ViewRows(sphere string) ([]index.Row, error) {
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	rows, err := x.List(index.Filter{})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if !r.Sealed {
			out = append(out, r)
		}
	}
	return out, nil
}

// VersionFile is the content file of one version of an artefact.
type VersionFile struct {
	Version string
	Path    string
	Created time.Time
}

// VersionFiles lists the payload of every version of an unsealed artefact.
func (s *Store) VersionFiles(sphere, id string) ([]VersionFile, error) {
	r, err := s.Resolve(URI(sphere, id))
	if err != nil {
		return nil, err
	}
	if r.Row.Sealed {
		return nil, &NotFound{"sealed"}
	}
	inv, err := ocfl.ReadInventory(r.Dir)
	if err != nil {
		return nil, err
	}
	var out []VersionFile
	for _, v := range inv.VersionNames() {
		files, err := inv.Files(v)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if strings.HasPrefix(f.Logical, "payload") {
				created, _ := time.Parse(time.RFC3339, inv.Versions[v].Created)
				out = append(out, VersionFile{Version: v, Path: ocfl.ContentPath(r.Dir, f), Created: created})
			}
		}
	}
	return out, nil
}

// payloadExt is the extension of the payload's logical name, without .age.
func payloadExt(objDir string) string {
	inv, err := ocfl.ReadInventory(objDir)
	if err != nil {
		return ""
	}
	files, _ := inv.Files("")
	for _, f := range files {
		if strings.HasPrefix(f.Logical, "payload") {
			return filepath.Ext(strings.TrimSuffix(f.Logical, ".age"))
		}
	}
	return ""
}

// Archive marks an artefact archived, or active again: it stays kept,
// replicated and reachable by its id and view path, and leaves the lists.
func (s *Store) Archive(ref string, archived bool, reason string) (*Card, error) {
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
	if archived == (c.Archived != "") {
		return c, nil
	}
	at := s.now()
	typ := "unarchive"
	c.Archived = ""
	if archived {
		typ, c.Archived = "archive", at[:10]
	}
	var detail map[string]any
	if reason != "" {
		detail = map[string]any{"reason": reason}
	}
	if err := appendEvent(r.Dir, Event{At: at, Type: typ, Agent: s.Agent, By: s.By, Outcome: "ok", Detail: detail}); err != nil {
		return nil, err
	}
	return c, s.saveCard(r, c, at)
}

// DataVersions gives, per granted sphere, a number that changes when the
// sphere's index was written by someone else since the last call.
func (s *Store) DataVersions() map[string]int64 {
	out := map[string]int64{}
	for _, sp := range s.Spheres {
		x, err := s.index(sp)
		if err != nil {
			continue
		}
		if v, err := x.DataVersion(); err == nil {
			out[sp] = v
		}
	}
	return out
}

// Signatures gives, per granted sphere, what changes when artefacts or
// cards change; copies being recorded leave it as it is.
func (s *Store) Signatures() map[string]string {
	out := map[string]string{}
	for _, sp := range s.Spheres {
		x, err := s.index(sp)
		if err != nil {
			continue
		}
		if v, err := x.Signature(); err == nil {
			out[sp] = v
		}
	}
	return out
}
