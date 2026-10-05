package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/config"
)

// The semantic side of the search is qmd: one index per sphere, named
// artefact-<sphere>, over the text renditions only (a sealed artefact has
// none). artefact writes its configuration and keeps it up to date.

func (s *Store) qmdCommand() []string {
	if len(s.Cfg.Search.Qmd) > 0 {
		out := make([]string, len(s.Cfg.Search.Qmd))
		for i, a := range s.Cfg.Search.Qmd {
			out[i] = config.Expand(a)
		}
		return out
	}
	return []string{"qmd"}
}

// SemanticEnabled reports whether the configuration lets searches rank by
// meaning, and qmd is there.
func (s *Store) SemanticEnabled() bool {
	switch s.Cfg.Search.Semantic {
	case "none", "off", "false":
		return false
	}
	_, err := exec.LookPath(s.qmdCommand()[0])
	return err == nil
}

// SemanticReady reports whether the sphere has its index by meaning.
func (s *Store) SemanticReady(sphere string) bool { return s.semanticReady(sphere) }

// semanticReady reports whether the sphere has its qmd index, over this store.
func (s *Store) semanticReady(sphere string) bool {
	if !s.SemanticEnabled() {
		return false
	}
	b, err := os.ReadFile(qmdConfigPath(sphere))
	return err == nil && bytes.Contains(b, []byte("path: "+s.root(sphere)+"\n"))
}

func qmdIndex(sphere string) string { return "artefact-" + sphere }

// qmdConfigPath is where qmd reads the collections of an index.
func qmdConfigPath(sphere string) string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = config.Expand("~/.config")
	}
	return filepath.Join(dir, "qmd", qmdIndex(sphere)+".yml")
}

// writeQmdConfig declares the sphere's renditions to qmd, keeping the models
// section a user may have added.
func (s *Store) writeQmdConfig(sphere string) error {
	p := qmdConfigPath(sphere)
	body := fmt.Sprintf("collections:\n    %s:\n        path: %s\n        pattern: \"**/extensions/artefact/renditions/*.md\"\n", sphere, s.root(sphere))
	if old, err := os.ReadFile(p); err == nil {
		if i := bytes.Index(old, []byte("\nmodels:")); i >= 0 {
			body += string(old[i+1:])
		}
		if string(old) == body {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(body), 0o644)
}

var indexedRe = regexp.MustCompile(`Indexed: (\d+) new, (\d+) updated, \d+ unchanged, (\d+) removed`)

func (s *Store) qmd(ctx context.Context, sphere string, args ...string) ([]byte, error) {
	cmd := s.qmdCommand()
	full := append(append(append([]string{}, cmd[1:]...), "--index", qmdIndex(sphere)), args...)
	x := exec.CommandContext(ctx, cmd[0], full...)
	var stderr bytes.Buffer
	x.Stderr = &stderr
	out, err := x.Output()
	if err != nil {
		return out, fmt.Errorf("qmd %s: %v %s", args[0], err, strings.TrimSpace(lastLine(stderr.String())))
	}
	return out, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

type SemanticResult struct {
	Sphere  string `json:"sphere"`
	Changed bool   `json:"changed"`
	Pending string `json:"embedded,omitempty"`
}

// RefreshSemantic brings the sphere's qmd index up to date: new, changed and
// removed renditions, then the vectors of what changed.
func (s *Store) RefreshSemantic(ctx context.Context, sphere string) (*SemanticResult, error) {
	if err := s.writeQmdConfig(sphere); err != nil {
		return nil, err
	}
	out, err := s.qmd(ctx, sphere, "update")
	if err != nil {
		return nil, err
	}
	res := &SemanticResult{Sphere: sphere}
	if m := indexedRe.FindStringSubmatch(string(out)); m != nil {
		res.Changed = m[1] != "0" || m[2] != "0" || m[3] != "0"
	}
	if strings.Contains(string(out), "need vectors") {
		res.Changed = true
	}
	if res.Changed {
		if _, err := s.qmd(ctx, sphere, "embed"); err != nil {
			return nil, err
		}
	}
	return res, nil
}

type semanticHit struct {
	rank    int
	score   float64
	snippet string
}

// semanticSearch returns the artefacts of sphere whose text is closest in
// meaning to text, best first, by id.
func (s *Store) semanticSearch(sphere, text string, n int, paths map[string]string) (map[string]semanticHit, error) {
	if _, err := os.Stat(qmdConfigPath(sphere)); err != nil {
		return nil, fmt.Errorf("no semantic index for %s yet: artefact reindex --semantic", sphere)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.qmd(ctx, sphere, "vsearch", text, "--format", "json", "-n", fmt.Sprint(n))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Score   float64 `json:"score"`
		File    string  `json:"file"`
		Snippet string  `json:"snippet"`
	}
	if i := bytes.IndexByte(out, '['); i > 0 {
		out = out[i:]
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("qmd vsearch: unreadable answer: %w", err)
	}
	hits := map[string]semanticHit{}
	for _, r := range rows {
		rel := strings.TrimPrefix(r.File, "qmd://"+sphere+"/")
		if i := strings.IndexByte(rel, '?'); i >= 0 {
			rel = rel[:i]
		}
		obj, _, ok := strings.Cut(rel, "/extensions/")
		if !ok {
			continue
		}
		id, ok := paths[obj]
		if !ok {
			continue
		}
		if _, seen := hits[id]; seen {
			continue
		}
		hits[id] = semanticHit{rank: len(hits), score: r.Score, snippet: cleanSnippet(r.Snippet)}
	}
	return hits, nil
}

// cleanSnippet drops qmd's diff header and the layout spaces of a PDF text.
func cleanSnippet(s string) string {
	if strings.HasPrefix(s, "@@") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}
