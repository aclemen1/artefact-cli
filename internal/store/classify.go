package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aclemen1/jev"
	"gopkg.in/yaml.v3"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/index"
)

// Plans is where the classification plans live.
func (s *Store) Plans() classify.Store {
	return classify.Store{Dir: filepath.Join(s.state(), "plans")}
}

func (s *Store) loadPlan(id, sphere string) (*classify.Plan, error) {
	p, err := s.Plans().Load(id)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	if p.Sphere != sphere {
		return nil, userErr("plan %q belongs to sphere %q; this artefact is in %q", id, p.Sphere, sphere)
	}
	return p, nil
}

// jevClient opens the jev client once (~/.config/jev/config.yaml); Close releases it.
func (s *Store) jevClient() (*jev.Client, error) {
	if s.Jev != nil {
		return s.Jev, nil
	}
	c, err := jev.Load()
	if err != nil {
		return nil, userErr("%v", err)
	}
	s.Jev = c
	return c, nil
}

// Decider builds the named decider from the configuration.
func (s *Store) Decider(name string) (classify.Decider, error) {
	if name == "" {
		name = s.Cfg.Classify.Decider
	}
	switch name {
	case "", "openjev", "jev":
		c, err := s.jevClient()
		if err != nil {
			return nil, err
		}
		if name == "jev" {
			return &classify.Jev{Client: c}, nil // Jev, then OpenJev on refusal
		}
		return &classify.Jev{Client: c, Only: "openjev"}, nil
	case "acp":
		return classify.ACP{Command: s.Cfg.Classify.Deciders.ACP.Command}, nil
	case "rule":
		return nil, nil
	}
	return nil, userErr("unknown decider %q; deciders: rule, openjev, jev, acp", name)
}

// facts gathers what a decider may know of an artefact. A sealed artefact
// gives its card only.
func (s *Store) facts(r *Ref, c *Card) classify.Facts {
	card := map[string]any{"title": c.Title, "date": c.Date}
	if len(c.DC) > 0 {
		card["dc"] = c.DC
	}
	if len(c.X) > 0 {
		card["x"] = c.X
	}
	b, _ := yaml.Marshal(card)
	f := classify.Facts{Title: c.Title, Filename: c.Filename, Mime: c.Mime, Date: c.Date, Card: string(b)}
	if c.Sealed {
		return f
	}
	if p, err := payloadFile(r.Dir, ""); err == nil {
		f.File = p
		if t := renditionText(r.Dir, c); t != "" {
			rs := []rune(t)
			if len(rs) > 4000 {
				rs = rs[:4000]
			}
			f.Text = string(rs)
		} else if textual(c.Mime) {
			if fh, err := os.Open(p); err == nil {
				buf := make([]byte, 4000)
				n, _ := io.ReadFull(fh, buf)
				fh.Close()
				t := buf[:n]
				for len(t) > 0 && !utf8.Valid(t) {
					t = t[:len(t)-1]
				}
				f.Text = string(t)
			}
		}
	}
	return f
}

func textual(mime string) bool {
	return strings.HasPrefix(mime, "text/") || mime == "application/json" || strings.HasSuffix(mime, "+xml") || mime == "application/xml"
}

type ClassifyResult struct {
	URI        string          `json:"uri"`
	Plan       string          `json:"plan"`
	Status     string          `json:"status"` // classified | pending | kept_human
	Node       string          `json:"node,omitempty"`
	Label      string          `json:"label,omitempty"`
	By         string          `json:"by,omitempty"`
	Confidence float64         `json:"confidence"`
	Steps      []classify.Step `json:"steps,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// Classify places an artefact in plan: by the plan's rules first, then by
// the decider, level by level. A human decision is never overwritten.
func (s *Store) Classify(ctx context.Context, ref, planID, deciderName string) (*ClassifyResult, error) {
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	p, err := s.loadPlan(planID, r.Sphere)
	if err != nil {
		return nil, err
	}
	c, err := readCard(r.Dir)
	if err != nil {
		return nil, err
	}
	res := &ClassifyResult{URI: URI(r.Sphere, r.ID), Plan: p.ID}
	if k, ok := c.Classes[p.ID]; ok && ByHand(k.By) {
		res.Status, res.Node, res.By, res.Confidence = "kept_human", k.Node, k.By, k.Confidence
		return res, nil
	}
	f := s.facts(r, c)
	var cls Class
	if n := p.MatchRules(f); n != nil {
		cls = Class{Node: n.Notation, By: "rule:" + n.Notation, Confidence: 1}
	} else {
		d, err := s.Decider(deciderName)
		if err != nil {
			return nil, err
		}
		if d == nil { // rules only, and none matched
			cls = Class{By: "rule", Pending: true}
		} else {
			out, err := classify.Walk(ctx, p, f, d, s.Cfg.ClassifyThreshold())
			if err != nil {
				return nil, err
			}
			cls = Class{Node: out.Node, By: "decider:" + d.Name(), Confidence: out.Confidence, Pending: out.Pending}
			res.Steps = out.Steps
		}
	}
	unlock, err := s.lock(r.Sphere)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c, err = readCard(r.Dir); err != nil {
		return nil, err
	}
	at := s.now()
	cls.At = at
	if c.Classes == nil {
		c.Classes = map[string]Class{}
	}
	c.Classes[p.ID] = cls
	detail := map[string]any{"plan": p.ID, "node": cls.Node, "by": cls.By, "confidence": cls.Confidence, "pending": cls.Pending}
	if len(res.Steps) > 0 {
		detail["steps"] = res.Steps
	}
	if err := appendEvent(r.Dir, Event{At: at, Type: "classify", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: detail}); err != nil {
		return nil, err
	}
	if err := s.saveCard(r, c, at); err != nil {
		return nil, err
	}
	res.Status, res.Node, res.By, res.Confidence = "classified", cls.Node, cls.By, cls.Confidence
	if cls.Pending {
		res.Status = "pending"
	}
	if n, _ := p.Find(cls.Node); n != nil {
		res.Label = n.Label
	}
	return res, nil
}

// ByHand reports whether a class was set by an actor (human, agent or
// routine), not by a decider or a rule: a decider never overwrites it.
func ByHand(by string) bool {
	return by != "" && by != "rule" && !strings.HasPrefix(by, "decider:") && !strings.HasPrefix(by, "rule:")
}

// SetClass records a decision by hand, signed by (the actor); an empty node
// removes the class.
func (s *Store) SetClass(ref, planID, node, by string) (*ClassifyResult, error) {
	if by == "" {
		by = "unknown:cli"
	}
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	p, err := s.loadPlan(planID, r.Sphere)
	if err != nil {
		return nil, err
	}
	var n *classify.Node
	if node != "" {
		if n, _ = p.Find(node); n == nil {
			return nil, userErr("plan %q has no node %q; see `artefact plan show %s`", p.ID, node, p.ID)
		}
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
	at := s.now()
	res := &ClassifyResult{URI: URI(r.Sphere, r.ID), Plan: p.ID}
	if node == "" {
		delete(c.Classes, p.ID)
		res.Status = "removed"
	} else {
		if c.Classes == nil {
			c.Classes = map[string]Class{}
		}
		c.Classes[p.ID] = Class{Node: node, By: by, Confidence: 1, At: at}
		res.Status, res.Node, res.Label, res.By, res.Confidence = "classified", node, n.Label, by, 1
	}
	if err := appendEvent(r.Dir, Event{At: at, Type: "classify", Agent: s.Agent, By: s.By, Outcome: "ok", Detail: map[string]any{"plan": p.ID, "node": node, "by": by}}); err != nil {
		return nil, err
	}
	return res, s.saveCard(r, c, at)
}

// Due is an artefact whose retention has ended.
type Due struct {
	URI    string `json:"uri"`
	Title  string `json:"title"`
	Plan   string `json:"plan"`
	Node   string `json:"node"`
	Label  string `json:"label"`
	RuleAt string `json:"rule_at"` // node that carries the rule (self or ancestor)
	Keep   string `json:"keep"`
	Then   string `json:"then"` // archive, review or destroy
	Due    string `json:"due"`
	Held   bool   `json:"held,omitempty"` // a hold suspends the disposal
	Done   string `json:"done,omitempty"` // archived, when review archived it
}

// Review lists the artefacts whose retention ends before `before`. With
// several plans, the longest keep wins: an artefact is due only in the plan
// that keeps it longest. Nothing is destroyed here.
func (s *Store) Review(planID string, before time.Time) ([]Due, error) {
	plans, err := s.Plans().List()
	if err != nil {
		return nil, err
	}
	type cand struct {
		d   Due
		due time.Time
		end bool // false: kept forever by some plan
	}
	best := map[string]*cand{}
	for _, p := range plans {
		if !contains(s.Spheres, p.Sphere) || (planID != "" && p.ID != planID) {
			continue
		}
		x, err := s.index(p.Sphere)
		if err != nil {
			return nil, err
		}
		cls, err := x.Classified(p.ID)
		if err != nil {
			return nil, err
		}
		for id, k := range cls {
			if k.Node == "" {
				continue
			}
			ret, at := p.RetentionOf(k.Node)
			if ret == nil {
				continue
			}
			row, err := x.Get(id)
			if err != nil {
				continue
			}
			base := row.Date
			if ret.From == "deposit" {
				base = row.Deposited
			}
			bt, err := time.Parse("2006-01-02", base[:min(10, len(base))])
			if err != nil {
				continue
			}
			due, ends := ret.Due(bt)
			n, _ := p.Find(k.Node)
			cd := &cand{d: Due{URI: URI(p.Sphere, id), Title: row.Title, Plan: p.ID, Node: k.Node, Label: n.Label, RuleAt: at, Keep: ret.Keep, Then: ret.Then, Due: due.Format("2006-01-02")}, due: due, end: ends && ret.Then != "keep"}
			cur := best[cd.d.URI]
			switch {
			case cur == nil:
				best[cd.d.URI] = cd
			case !cd.end || (cur.end && cd.due.After(cur.due)):
				best[cd.d.URI] = cd
			}
		}
	}
	out := []Due{}
	for _, cd := range best {
		if !cd.end || cd.due.After(before) {
			continue
		}
		if row, err := s.rowOf(cd.d.URI); err == nil && row.Archived != "" && cd.d.Then == "archive" {
			continue // already done
		}
		if r, err := s.Resolve(cd.d.URI); err == nil {
			if c, err := readCard(r.Dir); err == nil && len(c.Holds) > 0 {
				cd.d.Held = true
			}
		}
		out = append(out, cd.d)
	}
	sortDue(out)
	return out, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func sortDue(l []Due) {
	for i := range l {
		for j := i + 1; j < len(l); j++ {
			if l[j].Due < l[i].Due {
				l[i], l[j] = l[j], l[i]
			}
		}
	}
}

func (s *Store) rowOf(uri string) (index.Row, error) {
	r, err := s.Resolve(uri)
	if err != nil {
		return index.Row{}, err
	}
	return r.Row, nil
}

// ApplyArchive archives the due artefacts whose disposition is archive,
// unless a hold suspends it.
func (s *Store) ApplyArchive(due []Due) ([]Due, error) {
	for i, d := range due {
		if d.Then != "archive" || d.Held {
			continue
		}
		if _, err := s.Archive(d.URI, true, "retention "+d.Plan+":"+d.RuleAt+" ended "+d.Due); err != nil {
			return due, err
		}
		due[i].Done = "archived"
	}
	return due, nil
}

// ClassMembers gives the place of every artefact of sphere in plan.
func (s *Store) ClassMembers(sphere, plan string) (map[string]index.ClassRow, error) {
	x, err := s.index(sphere)
	if err != nil {
		return nil, err
	}
	return x.Classified(plan)
}
