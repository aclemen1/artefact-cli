package store

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/meta"
)

// clearCopy writes the current payload, decrypted if sealed, into the
// sphere's staging area under its file name, and returns the path.
func (s *Store) clearCopy(r *Ref, c *Card) (string, func(), error) {
	rc, _, err := s.OpenPayload(r.ID, "", c.Sealed)
	if err != nil {
		return "", nil, err
	}
	defer rc.Close()
	dir, err := os.MkdirTemp(filepath.Join(s.root(r.Sphere), "extensions", "artefact-staging"), "meta-")
	if err != nil {
		return "", nil, err
	}
	done := func() { os.RemoveAll(dir) }
	name := "payload" + c.Ext
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		done()
		return "", nil, err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		done()
		return "", nil, err
	}
	return p, done, f.Close()
}

type MetaResult struct {
	ID       string         `json:"id"`
	URI      string         `json:"uri"`
	Embedded *meta.Embedded `json:"embedded,omitempty"`
	Date     string         `json:"date"`
	Version  int            `json:"version,omitempty"` // with write: the new version
	Note     string         `json:"note,omitempty"`
}

// ReadMeta reads the metadata embedded in the payload into the card. When
// the card's date is still the deposit date, the content's own date
// replaces it.
func (s *Store) ReadMeta(ref string) (*MetaResult, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if !meta.Available() {
		return nil, userErr("exiftool is not installed (brew install exiftool)")
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	if c.Sealed {
		// The card stays in clear: a sealed file's own metadata (place,
		// author, title) does not go into it.
		return &MetaResult{ID: c.ID, URI: URI(r.Sphere, c.ID), Date: c.Date, Note: "sealed: its embedded metadata is not copied into the card"}, nil
	}
	p, done, err := s.clearCopy(r, c)
	if err != nil {
		return nil, err
	}
	e, err := meta.Read(p)
	done()
	if err != nil {
		return nil, err
	}
	res := &MetaResult{ID: c.ID, URI: URI(r.Sphere, c.ID)}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c, err = readCard(r.Dir); err != nil {
		return nil, err
	}
	if e.Empty() {
		c.Embedded = nil
	} else {
		c.Embedded = &e
		if e.Date != "" && strings.HasPrefix(c.Deposited, c.Date) {
			c.Date = e.Date
			res.Note = "date taken from the file"
		}
	}
	at := s.now()
	if err := appendEvent(r.Dir, Event{At: at, Type: "meta", Agent: s.Agent, Outcome: "ok"}); err != nil {
		return nil, err
	}
	res.Embedded, res.Date = c.Embedded, c.Date
	return res, s.saveCard(r, c, at)
}

// WriteMeta writes the card's title, date, creator, description and tags
// into the payload as XMP, as a new version.
func (s *Store) WriteMeta(ref string) (*MetaResult, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	if !meta.Writable(c.Ext) {
		return nil, userErr("artefact writes metadata into images, PDF and videos only, not %s files", c.Ext)
	}
	p, done, err := s.clearCopy(r, c)
	if err != nil {
		return nil, err
	}
	defer done()
	str := func(v any) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(fmtAny(v)), "[]"))
	}
	f := meta.Fields{Title: c.Title, Date: c.Date, Creator: str(c.DC["creator"]), Description: str(c.DC["description"]), Keywords: c.Tags}
	if err := meta.Write(p, f); err != nil {
		return nil, userErr("%v", err)
	}
	v, err := s.newVersion(ref, p, false, "meta-write")
	if err != nil {
		return nil, err
	}
	return &MetaResult{ID: c.ID, URI: URI(r.Sphere, c.ID), Date: c.Date, Version: v.Version}, nil
}

// Tag adds and removes tags.
func (s *Store) Tag(ref string, add, remove []string) (*Card, error) {
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
	c.addTags(add)
	c.removeTags(remove)
	at := s.now()
	if err := appendEvent(r.Dir, Event{At: at, Type: "card", Agent: s.Agent, Outcome: "ok", Detail: map[string]any{"fields": []string{"tags"}}}); err != nil {
		return nil, err
	}
	return c, s.saveCard(r, c, at)
}

// TagCounts counts the tags of the granted spheres.
func (s *Store) TagCounts(sphere string) (map[string]int, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	out := map[string]int{}
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		m, err := x.Tags()
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] += v
		}
	}
	return out, nil
}
