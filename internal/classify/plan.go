// Package classify holds the classification plans and the deciders that
// place an artefact in them (SPEC §9).
package classify

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Plan is a hierarchical classification plan of one sphere.
type Plan struct {
	ID     string  `yaml:"id" json:"id"`
	Title  string  `yaml:"title" json:"title"`
	Sphere string  `yaml:"sphere" json:"sphere"`
	Nodes  []*Node `yaml:"nodes" json:"nodes"`
}

// Node is one class. Its scope defines it, for humans and for deciders.
type Node struct {
	Notation  string     `yaml:"notation" json:"notation"`
	Label     string     `yaml:"label" json:"label"`
	Scope     string     `yaml:"scope,omitempty" json:"scope,omitempty"`
	Retention *Retention `yaml:"retention,omitempty" json:"retention,omitempty"`
	Rules     []Rule     `yaml:"rules,omitempty" json:"rules,omitempty"`
	Children  []*Node    `yaml:"children,omitempty" json:"children,omitempty"`
}

// Retention says how long an artefact of the class is kept, and then what.
type Retention struct {
	Keep string `yaml:"keep" json:"keep"`                     // 10y, 6m, 30d, or forever
	From string `yaml:"from,omitempty" json:"from,omitempty"` // date (of the content, default) or deposit
	Then string `yaml:"then" json:"then"`                     // keep, archive, review or destroy
}

// Rule places an artefact without a decider when every field it sets matches.
type Rule struct {
	Mime     string `yaml:"mime,omitempty" json:"mime,omitempty"`         // glob, e.g. image/*
	Filename string `yaml:"filename,omitempty" json:"filename,omitempty"` // regular expression, case-insensitive
	Title    string `yaml:"title,omitempty" json:"title,omitempty"`       // regular expression, case-insensitive
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Parse reads a plan and checks it.
func Parse(b []byte) (*Plan, error) {
	var p Plan
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return &p, p.Validate()
}

func (p *Plan) Validate() error {
	if !idRe.MatchString(p.ID) {
		return fmt.Errorf("plan id %q must be lower-case letters, digits and dashes", p.ID)
	}
	if p.Sphere == "" {
		return fmt.Errorf("plan %s: sphere is required", p.ID)
	}
	if len(p.Nodes) == 0 {
		return fmt.Errorf("plan %s has no node", p.ID)
	}
	seen := map[string]bool{}
	var check func(n *Node) error
	check = func(n *Node) error {
		if n.Notation == "" || n.Label == "" {
			return fmt.Errorf("plan %s: every node needs a notation and a label", p.ID)
		}
		if n.Notation == "none" || seen[n.Notation] {
			return fmt.Errorf("plan %s: notation %q is reserved or used twice", p.ID, n.Notation)
		}
		seen[n.Notation] = true
		if r := n.Retention; r != nil {
			if _, _, _, _, err := parseKeep(r.Keep); err != nil {
				return fmt.Errorf("plan %s, node %s: %v", p.ID, n.Notation, err)
			}
			if r.From != "" && r.From != "date" && r.From != "deposit" {
				return fmt.Errorf("plan %s, node %s: retention.from is date or deposit, got %q", p.ID, n.Notation, r.From)
			}
			if r.Then != "keep" && r.Then != "archive" && r.Then != "review" && r.Then != "destroy" {
				return fmt.Errorf("plan %s, node %s: retention.then is keep, archive, review or destroy, got %q", p.ID, n.Notation, r.Then)
			}
		}
		for _, rl := range n.Rules {
			if rl.Mime == "" && rl.Filename == "" && rl.Title == "" {
				return fmt.Errorf("plan %s, node %s: a rule needs mime, filename or title", p.ID, n.Notation)
			}
			for _, re := range []string{rl.Filename, rl.Title} {
				if _, err := regexp.Compile("(?i)" + re); err != nil {
					return fmt.Errorf("plan %s, node %s: %v", p.ID, n.Notation, err)
				}
			}
		}
		for _, c := range n.Children {
			if err := check(c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range p.Nodes {
		if err := check(n); err != nil {
			return err
		}
	}
	return nil
}

// Find returns the node of notation and its ancestors, root first.
func (p *Plan) Find(notation string) (*Node, []*Node) {
	var walk func(ns []*Node, up []*Node) (*Node, []*Node)
	walk = func(ns []*Node, up []*Node) (*Node, []*Node) {
		for _, n := range ns {
			if n.Notation == notation {
				return n, up
			}
			if f, a := walk(n.Children, append(append([]*Node{}, up...), n)); f != nil {
				return f, a
			}
		}
		return nil, nil
	}
	return walk(p.Nodes, nil)
}

// RetentionOf is the node's rule, inherited from the nearest ancestor.
func (p *Plan) RetentionOf(notation string) (*Retention, string) {
	n, up := p.Find(notation)
	if n == nil {
		return nil, ""
	}
	if n.Retention != nil {
		return n.Retention, n.Notation
	}
	for i := len(up) - 1; i >= 0; i-- {
		if up[i].Retention != nil {
			return up[i].Retention, up[i].Notation
		}
	}
	return nil, ""
}

// Facts are what a rule or a decider knows of an artefact.
type Facts struct {
	Title, Filename, Mime, Date string
	Card                        string // the card, as YAML
	Text                        string // an excerpt of the content, when readable
	File                        string // a local path to the payload, for deciders that open files
}

// MatchRules returns the deepest node whose rule matches, if any.
func (p *Plan) MatchRules(f Facts) *Node {
	var best *Node
	depth := -1
	var walk func(ns []*Node, d int)
	walk = func(ns []*Node, d int) {
		for _, n := range ns {
			for _, r := range n.Rules {
				if r.matches(f) && d > depth {
					best, depth = n, d
				}
			}
			walk(n.Children, d+1)
		}
	}
	walk(p.Nodes, 0)
	return best
}

func (r Rule) matches(f Facts) bool {
	if r.Mime != "" {
		if ok, _ := path.Match(r.Mime, f.Mime); !ok {
			return false
		}
	}
	for _, c := range [][2]string{{r.Filename, f.Filename}, {r.Title, f.Title}} {
		if c[0] != "" && !regexp.MustCompile("(?i)"+c[0]).MatchString(c[1]) {
			return false
		}
	}
	return true
}

// parseKeep reads 10y, 6m, 30d or forever; forever is a zero duration.
func parseKeep(s string) (years, months, days int, forever bool, err error) {
	if s == "forever" {
		return 0, 0, 0, true, nil
	}
	if len(s) < 2 {
		return 0, 0, 0, false, fmt.Errorf("retention.keep takes 10y, 6m, 30d or forever, got %q", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, 0, 0, false, fmt.Errorf("retention.keep takes 10y, 6m, 30d or forever, got %q", s)
	}
	switch s[len(s)-1] {
	case 'y':
		return n, 0, 0, false, nil
	case 'm':
		return 0, n, 0, false, nil
	case 'd':
		return 0, 0, n, false, nil
	}
	return 0, 0, 0, false, fmt.Errorf("retention.keep takes 10y, 6m, 30d or forever, got %q", s)
}

// Due is the end of retention from base; ok is false for forever.
func (r *Retention) Due(base time.Time) (time.Time, bool) {
	y, m, d, forever, err := parseKeep(r.Keep)
	if err != nil || forever {
		return time.Time{}, false
	}
	return base.AddDate(y, m, d), true
}

// Store keeps the plans as YAML files in a directory.
type Store struct{ Dir string }

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+".yaml") }

func (s Store) Load(id string) (*Plan, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no plan %q; see `artefact plan ls`", id)
		}
		return nil, err
	}
	return Parse(b)
}

func (s Store) List() ([]*Plan, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return []*Plan{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []*Plan{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			p, err := s.Load(strings.TrimSuffix(e.Name(), ".yaml"))
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Save writes a checked plan; replace allows an existing id.
func (s Store) Save(raw []byte, replace bool) (*Plan, error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.path(p.ID)); err == nil && !replace {
		return nil, fmt.Errorf("plan %q exists; pass --replace to replace it", p.ID)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	return p, os.WriteFile(s.path(p.ID), raw, 0o644)
}

func (s Store) Remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no plan %q", id)
		}
		return err
	}
	return nil
}

// SKOS renders the plan as a SKOS concept scheme in Turtle.
func (p *Plan) SKOS(base string) string {
	if base == "" {
		base = "urn:artefact:plan:" + p.ID + ":"
	}
	q := func(s string) string { return strconvQuote(s) }
	var b strings.Builder
	b.WriteString("@prefix skos: <http://www.w3.org/2004/02/skos/core#> .\n")
	b.WriteString("@prefix art: <https://github.com/aclemen1/artefact-cli/ns#> .\n\n")
	fmt.Fprintf(&b, "<%s> a skos:ConceptScheme ;\n  skos:prefLabel %s .\n\n", base, q(p.Title))
	var walk func(ns []*Node, parent string)
	walk = func(ns []*Node, parent string) {
		for _, n := range ns {
			fmt.Fprintf(&b, "<%s%s> a skos:Concept ;\n  skos:inScheme <%s> ;\n  skos:notation %s ;\n  skos:prefLabel %s", base, n.Notation, base, q(n.Notation), q(n.Label))
			if n.Scope != "" {
				fmt.Fprintf(&b, " ;\n  skos:scopeNote %s", q(n.Scope))
			}
			if parent == "" {
				fmt.Fprintf(&b, " ;\n  skos:topConceptOf <%s>", base)
			} else {
				fmt.Fprintf(&b, " ;\n  skos:broader <%s%s>", base, parent)
			}
			if r := n.Retention; r != nil {
				fmt.Fprintf(&b, " ;\n  art:keep %s ;\n  art:then %s", q(r.Keep), q(r.Then))
			}
			b.WriteString(" .\n\n")
			walk(n.Children, n.Notation)
		}
	}
	walk(p.Nodes, "")
	return b.String()
}

func strconvQuote(s string) string { return strconv.Quote(s) }
