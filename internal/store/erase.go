package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/ocfl"
)

// EraseRequest is what the user asks to erase.
type EraseRequest struct {
	Refs    []string `json:"refs"`
	Version string   `json:"version,omitempty"` // one version of a single artefact
	Force   bool     `json:"force,omitempty"`   // erase despite holders
}

type PlanItem struct {
	URI       string         `json:"uri"`
	Title     string         `json:"title"`
	Whole     bool           `json:"whole"`
	Versions  []string       `json:"versions"`
	Holders   []index.Holder `json:"holders"`
	Locations []Location     `json:"locations"`
	digests   []string
	rel       string // object root, relative to the sphere's storage root
}

type Plan struct {
	Items   []PlanItem `json:"items"`
	Digest  string     `json:"digest"`
	Code    string     `json:"code,omitempty"`
	Expires string     `json:"expires,omitempty"`
}

type pending struct {
	Request EraseRequest `json:"request"`
	Spheres []string     `json:"spheres"`
	Digest  string       `json:"digest"`
	Expires time.Time    `json:"expires"`
}

func (s *Store) plan(req EraseRequest) (*Plan, error) {
	if len(req.Refs) == 0 {
		return nil, userErr("name the artefacts to erase. Example: artefact erase 01K9B3T0Z6Q8M2C4R7V1X5N9PA")
	}
	if req.Version != "" && len(req.Refs) != 1 {
		return nil, userErr("--version erases one version of one artefact; got %d artefacts. Example: artefact erase 01K9B3T0Z6Q8M2C4R7V1X5N9PA --version v1", len(req.Refs))
	}
	p := &Plan{Items: []PlanItem{}}
	seen := map[string]bool{}
	for _, ref := range req.Refs {
		r, err := s.Resolve(ref)
		if err != nil {
			return nil, err
		}
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		c, err := readCard(r.Dir)
		if err != nil {
			return nil, err
		}
		inv, err := ocfl.ReadInventory(r.Dir)
		if err != nil {
			return nil, err
		}
		names := inv.VersionNames()
		it := PlanItem{URI: URI(r.Sphere, r.ID), Title: c.Title, Whole: true, Versions: names, Holders: c.Holders, rel: filepath.ToSlash(r.Row.Path),
			Locations: []Location{{Backend: "local", Path: r.Dir}}}
		remote, err := s.remoteLocations(r.Sphere, r.Row.Path)
		if err != nil {
			return nil, err
		}
		it.Locations = append(it.Locations, remote...)
		if req.Version != "" {
			if !slices.Contains(names, req.Version) {
				return nil, userErr("artefact %s has no version %s; its versions: %s", r.ID, req.Version, strings.Join(names, ", "))
			}
			if len(names) > 1 {
				it.Whole, it.Versions = false, []string{req.Version}
			}
		}
		if len(c.Holders) > 0 && !req.Force {
			var hs []string
			for _, h := range c.Holders {
				hs = append(hs, h.By+":"+h.Ref)
			}
			return nil, userErr("artefact %s is held by %s; holders release it first, or a human passes --force", r.ID, strings.Join(hs, ", "))
		}
		for _, v := range it.Versions {
			files, err := inv.Files(v)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				it.digests = append(it.digests, v+":"+f.SHA512)
			}
		}
		p.Items = append(p.Items, it)
	}
	sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].URI < p.Items[j].URI })
	h := sha256.New()
	for _, it := range p.Items {
		b, _ := json.Marshal(struct {
			URI       string
			Whole     bool
			Versions  []string
			Digests   []string
			Locations []Location
			Holders   []index.Holder
		}{it.URI, it.Whole, it.Versions, it.digests, it.Locations, it.Holders})
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	p.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return p, nil
}

// PlanErase builds the plan and a single-use confirmation code.
func (s *Store) PlanErase(req EraseRequest) (*Plan, error) {
	p, err := s.plan(req)
	if err != nil {
		return nil, err
	}
	code := newCode()
	exp := s.Now().Add(s.Cfg.CodeTTL())
	b, _ := json.Marshal(pending{Request: req, Spheres: s.Spheres, Digest: p.Digest, Expires: exp})
	dir := filepath.Join(s.state(), "erase")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, codeFile(code)), b, 0o600); err != nil {
		return nil, err
	}
	p.Code, p.Expires = code, exp.Format(time.RFC3339)
	return p, nil
}

type ErasedItem struct {
	URI            string `json:"uri"`
	Whole          bool   `json:"whole"`
	VersionsErased int    `json:"versions_erased"`
}

type PurgedLocation struct {
	URI      string `json:"uri"`
	Backend  string `json:"backend"`
	Verified bool   `json:"verified"`
}

// Proof says what was erased, without names or titles.
type Proof struct {
	Erased     []ErasedItem     `json:"erased"`
	Locations  []PurgedLocation `json:"locations"`
	NotCovered []string         `json:"not_covered"`
}

// ConfirmErase runs the plan the code was issued for, if nothing changed.
func (s *Store) ConfirmErase(code string) (*Proof, error) {
	p := filepath.Join(s.state(), "erase", codeFile(strings.TrimSpace(code)))
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, userErr("unknown or already used erasure code %q; run `artefact erase <id>` again for a new plan", code)
	}
	os.Remove(p) // single use, whatever happens next
	var pd pending
	if err := json.Unmarshal(b, &pd); err != nil {
		return nil, err
	}
	if s.Now().After(pd.Expires) {
		return nil, userErr("erasure code %q expired at %s; run `artefact erase` again for a new plan", code, pd.Expires.Format(time.RFC3339))
	}
	for _, sp := range pd.Spheres {
		if !slices.Contains(s.Spheres, sp) {
			return nil, userErr("this erasure was planned with sphere %q, which is not granted here", sp)
		}
	}
	plan, err := s.plan(pd.Request)
	if err != nil {
		return nil, err
	}
	if plan.Digest != pd.Digest {
		return nil, userErr("the artefacts changed since the plan was made; nothing was erased. Run `artefact erase` again and review the new plan")
	}
	proof := &Proof{Erased: []ErasedItem{}, Locations: []PurgedLocation{}, NotCovered: notCovered}
	for _, it := range plan.Items {
		e, locs, err := s.eraseItem(it)
		if err != nil {
			return proof, err
		}
		proof.Erased = append(proof.Erased, e)
		proof.Locations = append(proof.Locations, locs...)
	}
	return proof, nil
}

func (s *Store) eraseItem(it PlanItem) (ErasedItem, []PurgedLocation, error) {
	e, loc, err := s.eraseLocal(it)
	if err != nil {
		return e, []PurgedLocation{loc}, err
	}
	locs := []PurgedLocation{loc}
	sphere, _, _ := strings.Cut(strings.TrimPrefix(it.URI, "artefact://"), "/")
	ctx := context.Background()
	for _, st := range s.syncObject(ctx, sphere, it.rel) {
		pl := PurgedLocation{URI: it.URI, Backend: st.Backend, Verified: len(st.Errors) == 0}
		if pl.Verified && it.Whole {
			if left, err := s.BackendFiles(ctx, sphere, st.Backend); err == nil {
				for _, f := range left {
					if strings.HasPrefix(f, it.rel+"/") {
						pl.Verified = false
						break
					}
				}
			}
		}
		locs = append(locs, pl)
	}
	return e, locs, nil
}

func (s *Store) eraseLocal(it PlanItem) (ErasedItem, PurgedLocation, error) {
	r, err := s.Resolve(it.URI)
	if err != nil {
		return ErasedItem{}, PurgedLocation{}, err
	}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return ErasedItem{}, PurgedLocation{}, err
	}
	defer unlock()
	root := s.root(r.Sphere)
	loc := PurgedLocation{URI: it.URI, Backend: "local"}
	if it.Whole {
		if err := ocfl.Remove(root, r.Dir); err != nil {
			return ErasedItem{}, loc, err
		}
		x, err := s.index(r.Sphere)
		if err != nil {
			return ErasedItem{}, loc, err
		}
		if err := x.Delete(r.ID); err != nil {
			return ErasedItem{}, loc, err
		}
		if err := s.tombstone(r.Sphere, r.ID); err != nil {
			return ErasedItem{}, loc, err
		}
		_, statErr := os.Stat(r.Dir)
		loc.Verified = errors.Is(statErr, os.ErrNotExist)
		return ErasedItem{URI: it.URI, Whole: true, VersionsErased: len(it.Versions)}, loc, nil
	}
	inv, err := ocfl.ReadInventory(r.Dir)
	if err != nil {
		return ErasedItem{}, loc, err
	}
	gone := map[string]bool{}
	for _, v := range it.Versions {
		gone[v] = true
	}
	versions, renumber, err := versionsOf(r.Dir, inv, func(v string) bool { return !gone[v] })
	if err != nil {
		return ErasedItem{}, loc, err
	}
	newInv, err := ocfl.Rebuild(root, r.Dir, versions)
	if err != nil {
		return ErasedItem{}, loc, err
	}
	if err := renumberEvents(r.Dir, renumber); err != nil {
		return ErasedItem{}, loc, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return ErasedItem{}, loc, err
	}
	at := s.now()
	c.Version, c.Digest = len(versions), headDigest(newInv)
	if err := appendEvent(r.Dir, Event{At: at, Type: "erase", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"versions_erased": len(it.Versions)}}); err != nil {
		return ErasedItem{}, loc, err
	}
	if err := s.saveCard(r, c, at); err != nil {
		return ErasedItem{}, loc, err
	}
	// Verified: no erased version is left in the rebuilt object.
	loc.Verified = len(newInv.VersionNames()) == len(inv.VersionNames())-len(it.Versions)
	return ErasedItem{URI: it.URI, Whole: false, VersionsErased: len(it.Versions)}, loc, nil
}

// tombstone records that an id existed and was erased, and nothing else.
func (s *Store) tombstone(sphere, id string) error {
	f, err := os.OpenFile(filepath.Join(s.state(), "tombstones-"+sphere+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]string{"id": id, "sphere": sphere, "erased": s.now()})
	_, err = f.Write(append(b, '\n'))
	return err
}

// Erased reports whether id was erased in sphere.
func (s *Store) Erased(sphere, id string) bool {
	b, err := os.ReadFile(filepath.Join(s.state(), "tombstones-"+sphere+".jsonl"))
	return err == nil && strings.Contains(string(b), `"id":"`+id+`"`)
}

const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(x)%len(codeAlphabet)])
	}
	return string(out)
}

// codeFile names a pending plan by the hash of its code, so the code itself
// is never written to disk.
func codeFile(code string) string {
	h := sha256.Sum256([]byte(strings.ToUpper(code)))
	return hex.EncodeToString(h[:]) + ".json"
}
