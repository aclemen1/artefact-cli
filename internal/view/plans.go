package view

import (
	"golang.org/x/text/unicode/norm"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/index"
)

// The plans view files every unsealed, active artefact under its title, in
// a folder per plan node:
//
//	plans/<plan>/<notation> <label>/…/<title><ext>
//	plans/<plan>/_unclassified/<title><ext>
const unclassified = "_unclassified"

type pnode struct {
	dir      bool
	children map[string]*pnode
	id       string // artefact, for a file
}

func newDir() *pnode { return &pnode{dir: true, children: map[string]*pnode{}} }

// buildPlans lays out the plans of the tree's sphere over rows.
func (t *Tree) buildPlans(rows map[string]index.Row) *pnode {
	root := newDir()
	plans, err := t.st.Plans().List()
	if err != nil {
		return root
	}
	for _, p := range plans {
		if p.Sphere != t.sphere {
			continue
		}
		members, err := t.st.ClassMembers(t.sphere, p.ID)
		if err != nil {
			continue
		}
		pd := newDir()
		root.children[safeName(p.ID)] = pd
		dirs := map[string]*pnode{}
		var walk func(ns []*classify.Node, parent *pnode)
		walk = func(ns []*classify.Node, parent *pnode) {
			for _, n := range ns {
				d := newDir()
				parent.children[safeName(n.Notation+" "+n.Label)] = d
				dirs[n.Notation] = d
				walk(n.Children, d)
			}
		}
		walk(p.Nodes, pd)
		un := newDir()
		pd.children[unclassified] = un
		ids := make([]string, 0, len(rows))
		for id := range rows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			r := rows[id]
			if r.Archived != "" {
				continue
			}
			target := un
			if m, ok := members[id]; ok && m.Node != "" {
				if d, ok := dirs[m.Node]; ok {
					target = d
				}
			}
			name := fileName(r)
			if _, taken := target.children[name]; taken {
				ext := path.Ext(name)
				name = strings.TrimSuffix(name, ext) + " (" + id[len(id)-6:] + ")" + ext
			}
			target.children[name] = &pnode{id: id}
		}
	}
	return root
}

// fileName is the artefact's title as a file name, with its extension.
func fileName(r index.Row) string {
	name := safeName(r.Title)
	if name == "" {
		name = r.ID
	}
	if r.Ext != "" && !strings.HasSuffix(strings.ToLower(name), strings.ToLower(r.Ext)) {
		name += r.Ext
	}
	return name
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == ':':
			return '-'
		case r < 0x20:
			return -1
		}
		return r
	}, s)
	s = norm.NFC.String(strings.TrimSpace(s))
	if s == "." || s == ".." {
		return "_"
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (t *Tree) plansNode(parts []string) (*pnode, map[string]index.Row, error) {
	rows, _, err := t.snapshot()
	if err != nil {
		return nil, nil, err
	}
	t.mu.Lock()
	if t.plans == nil || t.plansAt.Before(t.fetched) {
		t.plans, t.plansAt = t.buildPlans(rows), t.fetched
	}
	n := t.plans
	t.mu.Unlock()
	for _, p := range parts {
		if n == nil || !n.dir {
			return nil, nil, fs.ErrNotExist
		}
		n = n.children[p]
	}
	if n == nil {
		return nil, nil, fs.ErrNotExist
	}
	return n, rows, nil
}

func (t *Tree) statPlans(name string, parts []string) (*Info, error) {
	n, rows, err := t.plansNode(parts)
	if err != nil {
		return nil, err
	}
	if n.dir {
		d := t.dir(name)
		d.ident = "d:plans/" + strings.Join(parts, "/")
		return d, nil
	}
	r := rows[n.id]
	vs, err := t.st.VersionFiles(t.sphere, n.id)
	if err != nil || len(vs) == 0 {
		return nil, fs.ErrNotExist
	}
	return &Info{name: name, size: r.Size, mod: parseTime(r.Updated, t.started), real: vs[len(vs)-1].Path, ident: "a:" + n.id}, nil
}

func (t *Tree) readPlans(parts []string) ([]*Info, error) {
	n, rows, err := t.plansNode(parts)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, fs.ErrNotExist
	}
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]*Info, 0, len(names))
	for _, k := range names {
		c := n.children[k]
		if c.dir {
			d := t.dir(k)
			d.ident = "d:plans/" + strings.Join(append(append([]string{}, parts...), k), "/")
			out = append(out, d)
			continue
		}
		r := rows[c.id]
		out = append(out, &Info{name: k, size: r.Size, mod: parseTime(r.Updated, t.started), ident: "a:" + c.id})
	}
	return out, nil
}

// PlanPath is where the artefact id sits in the plan's tree of the view:
// plans/<plan>/<notation label>/…/<title><ext>, as the view names it.
func (t *Tree) PlanPath(plan, id string) (string, bool) {
	root, _, err := t.plansNode(nil)
	if err != nil {
		return "", false
	}
	pd := root.children[safeName(plan)]
	if pd == nil {
		return "", false
	}
	var find func(n *pnode, at []string) []string
	find = func(n *pnode, at []string) []string {
		for name, c := range n.children {
			p := append(append([]string(nil), at...), name)
			if !c.dir && c.id == id {
				return p
			}
			if c.dir {
				if r := find(c, p); r != nil {
					return r
				}
			}
		}
		return nil
	}
	p := find(pd, []string{"plans", safeName(plan)})
	if p == nil {
		return "", false
	}
	return path.Join(p...), true
}
