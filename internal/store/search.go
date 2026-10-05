package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

// maxIndexedText is how much of a text payload goes into the search index.
const maxIndexedText = 256 << 10

// searchDoc is the searchable text of an artefact: its card, and the
// content of a text payload or of a text rendition. A sealed artefact gives
// its card only.
func searchDoc(objDir string, c *Card) (meta, body string) {
	var m []string
	m = append(m, c.Mime, c.Date, c.Ext)
	for _, k := range sortedKeys(c.DC) {
		m = append(m, fmt.Sprint(c.DC[k]))
	}
	for _, k := range sortedKeys(c.X) {
		m = append(m, fmt.Sprint(c.X[k]))
	}
	for plan, k := range c.Classes {
		m = append(m, plan, k.Node)
	}
	if c.Sealed {
		return strings.Join(m, " "), ""
	}
	src := ""
	if p := filepath.Join(objDir, "v"+fmt.Sprint(c.Version), "content", "renditions", "text.md"); fileExists(p) {
		src = p
	} else if textual(c.Mime) {
		src, _ = payloadFile(objDir, "")
	}
	if src != "" {
		if f, err := os.Open(src); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, maxIndexedText))
			f.Close()
			for len(b) > 0 && !utf8.Valid(b) {
				b = b[:len(b)-1]
			}
			body = string(b)
		}
	}
	return strings.Join(m, " "), body
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) indexSearch(x *index.Index, objDir string, c *Card) error {
	meta, body := searchDoc(objDir, c)
	return x.PutSearch(c.ID, c.Title, c.Filename, meta, body)
}

type SearchHit struct {
	ListItem
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// Search finds artefacts of the granted spheres by words of their title,
// file name, card or text content.
func (s *Store) Search(q, sphere string, limit int, state string) ([]SearchHit, error) {
	if strings.TrimSpace(q) == "" {
		return nil, userErr("search needs words. Example: artefact search décompte assura")
	}
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	if limit <= 0 {
		limit = 20
	}
	out := []SearchHit{}
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		hits, err := x.Search(q, limit*2)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			row, err := x.Get(h.ID)
			if err != nil {
				continue
			}
			if (state == "active" && row.Archived != "") || (state == "archived" && row.Archived == "") {
				continue
			}
			out = append(out, SearchHit{ListItem: ListItem{Row: row, Sphere: sp, URI: URI(sp, row.ID)}, Snippet: h.Snippet, Score: h.Score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RebuildSearch fills the search index from the store without touching the
// other tables, so that it runs while the store is in use.
func (s *Store) RebuildSearch(sphere string) ([]ReindexResult, error) {
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
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		n := 0
		err = ocfl.Walk(s.root(sp), func(dir string) error {
			c, err := readCard(dir)
			if err != nil {
				return err
			}
			n++
			return s.indexSearch(x, dir, c)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, ReindexResult{Sphere: sp, Artefacts: n})
	}
	return out, nil
}

// WhichResult is what a lookup by content found in one sphere.
type WhichResult struct {
	Found    bool           `json:"found"`
	Sphere   string         `json:"sphere,omitempty"`
	ID       string         `json:"id,omitempty"`
	URI      string         `json:"uri,omitempty"`
	Title    string         `json:"title,omitempty"`
	Sealed   bool           `json:"sealed,omitempty"`
	Archived string         `json:"archived,omitempty"`
	Holders  []index.Holder `json:"holders,omitempty"`
	Digest   string         `json:"digest"`          // sha256 of the content looked up
	Checked  []string       `json:"checked"`         // spheres searched
	Notes    []string       `json:"notes,omitempty"` // e.g. a sphere whose sealed artefacts could not be checked
}

// Which finds, without writing anything, the artefact that holds exactly
// the content read from r: by its SHA-256 among clear artefacts, and by the
// keyed digest of the sphere among sealed ones (which needs the sphere's key).
func (s *Store) Which(r io.Reader, sphere string) (*WhichResult, error) {
	spheres := s.Spheres
	if sphere != "" {
		sp, err := s.sphereOrDefault(sphere)
		if err != nil {
			return nil, err
		}
		spheres = []string{sp}
	}
	h := sha256.New()
	writers := []io.Writer{h}
	keyed := map[string]func() string{}
	res := &WhichResult{Checked: spheres}
	for _, sp := range spheres {
		id, err := s.Keys.Identity(sp)
		if err != nil {
			res.Notes = append(res.Notes, "sphere "+sp+": no age key here, so sealed artefacts were not checked")
			continue
		}
		w, sum := keys.Keyed(keys.DedupKey(id))
		writers = append(writers, w)
		keyed[sp] = sum
	}
	if _, err := io.Copy(io.MultiWriter(writers...), r); err != nil {
		return nil, err
	}
	res.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	for _, sp := range spheres {
		x, err := s.index(sp)
		if err != nil {
			return nil, err
		}
		row, err := x.ByDigest(res.Digest)
		if errors.Is(err, index.ErrNotFound) && keyed[sp] != nil {
			row, err = x.ByKeyed(keyed[sp]())
		}
		if errors.Is(err, index.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		hs, err := x.Holders(row.ID)
		if err != nil {
			return nil, err
		}
		res.Found, res.Sphere, res.ID, res.URI, res.Title = true, sp, row.ID, URI(sp, row.ID), row.Title
		res.Sealed, res.Archived, res.Holders = row.Sealed, row.Archived, hs
		return res, nil
	}
	return res, nil
}
