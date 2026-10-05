package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aclemen1/artefact-cli/internal/index"
)

// Card is an artefact's metadata (SPEC §3.2). The core fields are written by
// artefact; title, date, filename, mime, dc and x by whoever deposits or edits.
type Card struct {
	ID        string         `yaml:"id" json:"id"`
	Sphere    string         `yaml:"sphere" json:"sphere"`
	Title     string         `yaml:"title" json:"title"`
	Date      string         `yaml:"date" json:"date"`
	Mime      string         `yaml:"mime" json:"mime"`
	Size      int64          `yaml:"size" json:"size"`
	Filename  string         `yaml:"filename,omitempty" json:"filename,omitempty"`
	Version   int            `yaml:"version" json:"version"`
	Digest    string         `yaml:"digest" json:"digest"`
	Sealed    bool           `yaml:"sealed" json:"sealed"`
	Deposited string         `yaml:"deposited" json:"deposited"`
	Updated   string         `yaml:"updated" json:"updated"`
	DC        map[string]any `yaml:"dc,omitempty" json:"dc,omitempty"`
	Classes   map[string]any `yaml:"classes,omitempty" json:"classes,omitempty"`
	Holders   []index.Holder `yaml:"holders" json:"holders"`
	Holds     []any          `yaml:"holds,omitempty" json:"holds,omitempty"`
	X         map[string]any `yaml:"x,omitempty" json:"x,omitempty"`
}

// Event is one line of the object's event log (SPEC §3.3).
type Event struct {
	At      string         `json:"at"`
	Type    string         `json:"type"`
	Agent   string         `json:"agent"`
	Outcome string         `json:"outcome"`
	Version int            `json:"version,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

const (
	extDir    = "extensions/artefact"
	cardFile  = "card.yaml"
	eventFile = "events.jsonl"
)

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Editable lists the card fields `card --set` and `--merge` may change.
var Editable = []string{"title", "date", "filename", "mime", "dc.<term>", "x.<namespace>.<key>"}

func readCard(objDir string) (*Card, error) {
	b, err := os.ReadFile(filepath.Join(objDir, extDir, cardFile))
	if err != nil {
		return nil, err
	}
	var c Card
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("card of %s: %w", objDir, err)
	}
	if c.Holders == nil {
		c.Holders = []index.Holder{}
	}
	return &c, nil
}

// writeCard replaces the card atomically.
func writeCard(objDir string, c *Card) error {
	dir := filepath.Join(objDir, extDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".card.yaml.tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, cardFile))
}

func appendEvent(objDir string, e Event) error {
	dir := filepath.Join(objDir, extDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, eventFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func readEvents(objDir string) ([]Event, error) {
	b, err := os.ReadFile(filepath.Join(objDir, extDir, eventFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("event log of %s: %w", objDir, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// addHolder records (by, ref) once and reports whether it was new.
func (c *Card) addHolder(h index.Holder) bool {
	for _, x := range c.Holders {
		if x.By == h.By && x.Ref == h.Ref {
			return false
		}
	}
	c.Holders = append(c.Holders, h)
	sort.SliceStable(c.Holders, func(i, j int) bool { return c.Holders[i].At < c.Holders[j].At })
	return true
}

// removeHolders drops the holders of by (only ref when given) and returns how many.
func (c *Card) removeHolders(by, ref string) int {
	n := 0
	c.Holders = slices.DeleteFunc(c.Holders, func(h index.Holder) bool {
		if h.By == by && (ref == "" || h.Ref == ref) {
			n++
			return true
		}
		return false
	})
	return n
}

// set changes one editable field from `field=value`.
func (c *Card) set(field, value string) error {
	switch {
	case field == "title":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("title cannot be empty. Example: artefact card <id> --set title=\"Ordonnance septembre 2026\"")
		}
		c.Title = value
	case field == "date":
		if !dateRe.MatchString(value) {
			return fmt.Errorf("date takes YYYY-MM-DD, got %q. Example: artefact card <id> --set date=2026-09-18", value)
		}
		c.Date = value
	case field == "filename":
		c.Filename = filepath.Base(value)
	case field == "mime":
		c.Mime = value
	case strings.HasPrefix(field, "dc.") && len(field) > 3:
		if c.DC == nil {
			c.DC = map[string]any{}
		}
		if value == "" {
			delete(c.DC, field[3:])
		} else {
			c.DC[field[3:]] = value
		}
	case strings.HasPrefix(field, "x."):
		ns, key, ok := strings.Cut(field[2:], ".")
		if !ok || ns == "" || key == "" {
			return fmt.Errorf("x fields take x.<namespace>.<key>, got %q. Example: artefact card <id> --set x.mnemo.note=\"pour la caisse AVS\"", field)
		}
		if c.X == nil {
			c.X = map[string]any{}
		}
		m, _ := c.X[ns].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		if value == "" {
			delete(m, key)
		} else {
			m[key] = value
		}
		if len(m) == 0 {
			delete(c.X, ns)
		} else {
			c.X[ns] = m
		}
	default:
		return fmt.Errorf("field %q cannot be edited; editable fields: %s. Example: artefact card <id> --set title=\"Ordonnance\"", field, strings.Join(Editable, ", "))
	}
	return nil
}

// merge applies a YAML mapping of editable fields: dc terms are merged one
// by one, x namespaces are replaced whole.
func (c *Card) merge(doc string) error {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		return fmt.Errorf("--merge takes a YAML mapping: %v. Example: artefact card <id> --merge 'dc: {creator: Dr X}'", err)
	}
	for k, v := range m {
		switch k {
		case "title", "date", "filename", "mime":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s takes a string, got %v", k, v)
			}
			if err := c.set(k, s); err != nil {
				return err
			}
		case "dc":
			dm, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("dc takes a mapping of Dublin Core terms, got %v", v)
			}
			if c.DC == nil {
				c.DC = map[string]any{}
			}
			for t, tv := range dm {
				if tv == nil {
					delete(c.DC, t)
				} else {
					c.DC[t] = tv
				}
			}
		case "x":
			xm, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("x takes a mapping of namespaces, got %v", v)
			}
			if c.X == nil {
				c.X = map[string]any{}
			}
			for ns, nv := range xm {
				if nv == nil {
					delete(c.X, ns)
				} else {
					c.X[ns] = nv
				}
			}
		default:
			return fmt.Errorf("field %q cannot be edited; editable fields: %s", k, strings.Join(Editable, ", "))
		}
	}
	return nil
}
