package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
)

// Rendition is what the card says of an artefact's text rendition, made by
// lector (SPEC §10). The text itself lives in the object's extensions, as
// derived data: an OCFL version stays as it was written.
type Rendition struct {
	Version    int               `yaml:"version" json:"version"`
	At         string            `yaml:"at" json:"at"`
	Chars      int               `yaml:"chars" json:"chars"`
	Confidence float64           `yaml:"confidence,omitempty" json:"confidence,omitempty"`
	Methods    map[string]string `yaml:"methods,omitempty" json:"methods,omitempty"`
	Truncated  bool              `yaml:"truncated,omitempty" json:"truncated,omitempty"`
	Blank      bool              `yaml:"blank,omitempty" json:"blank,omitempty"` // no text found (a photo)
	Error      string            `yaml:"error,omitempty" json:"error,omitempty"` // lector could not read it
}

// maxRenditionChars bounds a rendition: enough for search and deciders.
const maxRenditionChars = 200000

func renditionPath(objDir string, version int) string {
	return filepath.Join(objDir, extDir, "renditions", fmt.Sprintf("v%d.md", version))
}

// dropRenditions forgets the text of earlier content: a sealed artefact
// keeps no clear text, and a new version needs its own rendition.
func dropRenditions(objDir string, c *Card) error {
	c.Rendition = nil
	if err := os.RemoveAll(filepath.Join(objDir, extDir, "renditions")); err != nil {
		return err
	}
	return nil
}

// RenditionText returns the text rendition of an artefact's current version.
func (s *Store) RenditionText(ref string) (string, *Card, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return "", nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return "", nil, err
	}
	if c.Sealed {
		return "", c, userErr("%s is sealed: it has no text rendition", c.ID)
	}
	if c.Rendition == nil || c.Rendition.Version != c.Version {
		return "", c, userErr("%s has no text rendition yet. Example: artefact render %s", c.ID, c.ID)
	}
	if c.Rendition.Error != "" {
		return "", c, userErr("lector could not read %s: %s", c.ID, c.Rendition.Error)
	}
	return renditionText(r.Dir, c), c, nil
}

// renditionText returns the text rendition of the current version, if any.
func renditionText(objDir string, c *Card) string {
	if c.Rendition == nil || c.Rendition.Version != c.Version {
		return ""
	}
	b, err := os.ReadFile(renditionPath(objDir, c.Version))
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *Store) lectorCommand() []string {
	if len(s.Cfg.Lector.Command) > 0 {
		out := make([]string, len(s.Cfg.Lector.Command))
		for i, a := range s.Cfg.Lector.Command {
			out[i] = config.Expand(a)
		}
		return out
	}
	return []string{"lector"}
}

type RenderResult struct {
	ID        string     `json:"id"`
	URI       string     `json:"uri"`
	Status    string     `json:"status"` // rendered | current | sealed | failed
	Rendition *Rendition `json:"rendition,omitempty"`
}

// Render asks lector for the text of the current version and keeps it.
// A rendition already made for this version is kept unless force.
func (s *Store) Render(ref string, force bool) (*RenderResult, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	res := &RenderResult{ID: c.ID, URI: URI(r.Sphere, c.ID)}
	if c.Sealed {
		res.Status = "sealed"
		return res, nil
	}
	if !force && c.Rendition != nil && c.Rendition.Version == c.Version && c.Rendition.Error == "" {
		res.Status, res.Rendition = "current", c.Rendition
		return res, nil
	}
	payload, err := payloadFile(r.Dir, "")
	if err != nil {
		return nil, err
	}
	name := c.Filename
	if name == "" {
		name = "payload" + c.Ext
	}
	cmd := s.lectorCommand()
	args := append(cmd[1:], "read", payload, "--max-chars", fmt.Sprint(maxRenditionChars))
	var stdin *os.File
	if filepath.Ext(payload) == "" {
		// lector takes the format from the name; a bare payload goes through stdin.
		args = append(cmd[1:], "read", "-", "--filename", name, "--max-chars", fmt.Sprint(maxRenditionChars))
		if stdin, err = os.Open(payload); err != nil {
			return nil, err
		}
		defer stdin.Close()
	}
	var stdout, stderr bytes.Buffer
	x := exec.Command(cmd[0], args...)
	x.Stdout, x.Stderr = &stdout, &stderr
	if stdin != nil {
		x.Stdin = stdin
	}
	runErr := x.Run()
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Markdown   string            `json:"markdown"`
			Methods    map[string]string `json:"methods"`
			Confidence float64           `json:"confidence"`
			Range      struct {
				Truncated bool `json:"truncated"`
				Total     int  `json:"total"`
			} `json:"range"`
		} `json:"result"`
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	rend := &Rendition{Version: c.Version, At: s.now()}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("lector: %v %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("lector: unreadable answer: %w", err)
	}
	text := ""
	switch {
	case env.OK:
		text = env.Result.Markdown
		rend.Methods, rend.Confidence, rend.Truncated = env.Result.Methods, env.Result.Confidence, env.Result.Range.Truncated
		rend.Blank = strings.TrimSpace(text) == ""
		rend.Chars = len([]rune(text))
	case env.Error != nil:
		rend.Error = env.Error.Kind + ": " + env.Error.Message
	}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c, err = readCard(r.Dir); err != nil {
		return nil, err
	}
	if c.Version != rend.Version {
		res.Status = "failed"
		return res, nil // a new version arrived meanwhile; the next render takes it
	}
	if rend.Error == "" {
		p := renditionPath(r.Dir, c.Version)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return nil, err
		}
		// Older renditions describe versions the card no longer shows.
		entries, _ := os.ReadDir(filepath.Dir(p))
		for _, e := range entries {
			if e.Name() != filepath.Base(p) {
				os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
			}
		}
	}
	c.Rendition = rend
	if err := appendEvent(r.Dir, Event{At: rend.At, Type: "render", Agent: s.Agent, Outcome: map[bool]string{true: "ok", false: "failed"}[rend.Error == ""], Version: c.Version}); err != nil {
		return nil, err
	}
	if err := s.saveCard(r, c, rend.At); err != nil {
		return nil, err
	}
	res.Rendition = rend
	res.Status = "rendered"
	if rend.Error != "" {
		res.Status = "failed"
	}
	return res, nil
}

// Unrendered lists the unsealed artefacts of sphere without a current rendition.
func (s *Store) Unrendered(sphere string) ([]string, error) {
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	rows, err := x.List(indexAll())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		if row.Sealed {
			continue
		}
		c, err := readCard(filepath.Join(s.root(sphere), row.Path))
		if err != nil {
			continue
		}
		if c.Rendition == nil || c.Rendition.Version != c.Version {
			out = append(out, URI(sphere, row.ID))
		}
	}
	return out, nil
}

// RenderMissing renders what lacks a rendition, at most limit artefacts.
func (s *Store) RenderMissing(sphere string, limit int, deadline time.Time) (int, error) {
	ids, err := s.Unrendered(sphere)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if (limit > 0 && n >= limit) || (!deadline.IsZero() && time.Now().After(deadline)) {
			break
		}
		if _, err := s.Render(id, false); err == nil {
			n++
		}
	}
	return n, nil
}

func indexAll() index.Filter { return index.Filter{} }
