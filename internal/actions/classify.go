package actions

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func init() {
	spec.Register(&spec.Action{
		Category: "plans", Name: "plan",
		Summary:    "Add, list, show, export or remove the classification plans.",
		Discussion: "A plan is a YAML file: id, title, sphere, and nodes with notation, label, scope (the definition, also given to the deciders), retention {keep: 10y|6m|30d|forever, from: date|deposit, then: keep|review|destroy} inherited by children, rules {mime, filename, title} and children. export writes the plan as SKOS in Turtle.",
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Required: true, Enum: []string{"add", "ls", "show", "export", "rm"}, Help: "add, ls, show, export or rm"},
			{Name: "arg", Kind: spec.String, Positional: true, Help: "The plan's YAML file for add, its id otherwise."},
			{Name: "replace", Kind: spec.Bool, Help: "With add: replace a plan of the same id."},
		},
		Examples: []string{"artefact plan add household.yaml", "artefact plan ls", "artefact plan show household --format text", "artefact plan export household > household.ttl"},
		Effects:  []string{"add and rm write .artefact/plans/ in the store; classes already given stay on the cards."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				ps := s.Plans()
				switch ctx.Str("verb") {
				case "ls":
					l, err := ps.List()
					if err != nil {
						return nil, err
					}
					out := []map[string]any{}
					for _, p := range l {
						out = append(out, map[string]any{"id": p.ID, "title": p.Title, "sphere": p.Sphere, "nodes": count(p.Nodes)})
					}
					return out, nil
				case "add":
					b, err := os.ReadFile(config.Expand(ctx.Str("arg")))
					if err != nil {
						return nil, spec.UserError("cannot read the plan file %q: %v. Example: artefact plan add household.yaml", ctx.Str("arg"), err)
					}
					p, err := ps.Save(b, ctx.Bool("replace"))
					if err != nil {
						return nil, spec.UserError("%v", err)
					}
					if !contains(s.Cfg.Spheres, p.Sphere) {
						_ = ps.Remove(p.ID)
						return nil, spec.UserError("plan %q names sphere %q, which is not configured", p.ID, p.Sphere)
					}
					return map[string]any{"id": p.ID, "sphere": p.Sphere, "nodes": count(p.Nodes)}, nil
				case "rm":
					if err := ps.Remove(ctx.Str("arg")); err != nil {
						return nil, spec.UserError("%v", err)
					}
					return map[string]any{"removed": ctx.Str("arg")}, nil
				}
				p, err := ps.Load(ctx.Str("arg"))
				if err != nil {
					return nil, spec.NotFound("%v", err)
				}
				if ctx.Str("verb") == "export" {
					fmt.Fprint(ctx.Stdout, p.SKOS(""))
					return spec.Streamed{}, nil
				}
				return p, nil
			})
		},
		Text: func(w io.Writer, r any) {
			p, ok := r.(*classify.Plan)
			if !ok {
				fmt.Fprintf(w, "%v\n", r)
				return
			}
			fmt.Fprintf(w, "%s — %s (%s)\n", p.ID, p.Title, p.Sphere)
			var walk func(ns []*classify.Node, d int)
			walk = func(ns []*classify.Node, d int) {
				for _, n := range ns {
					ret := ""
					if n.Retention != nil {
						ret = fmt.Sprintf("  [keep %s, then %s]", n.Retention.Keep, n.Retention.Then)
					}
					fmt.Fprintf(w, "%s%s %s%s\n", strings.Repeat("  ", d+1), n.Notation, n.Label, ret)
					walk(n.Children, d+1)
				}
			}
			walk(p.Nodes, 0)
		},
	})
	spec.Register(&spec.Action{
		Category: "plans", Name: "classify", Right: "write",
		Summary:    "Place artefacts in a plan: by its rules, then by a decider, level by level; or record a human decision with --node.",
		Discussion: "Below classify.threshold (0.8 by default) the walk stops at the parent node and the artefact is pending: `artefact ls --pending` lists it for a human. A human decision (--node) is never overwritten by a decider. A decider sees the card, an excerpt of a text payload, and (acp) the file; of a sealed artefact, the card only.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.StringList, Positional: true, Required: true, Help: "Artefact ids or URIs."},
			{Name: "plan", Kind: spec.String, Required: true, Help: "Plan id."},
			{Name: "decider", Kind: spec.String, Enum: []string{"rule", "openjev", "jev", "acp"}, Help: "Decider; defaults to classify.decider, then openjev. rule uses the plan's rules only."},
			{Name: "node", Kind: spec.String, Help: "Human decision: the node's notation; empty string removes the class."},
		},
		Examples: []string{"artefact classify 01K9B3T0Z6Q8M2C4R7V1X5N9PA --plan household", "artefact classify 01K9B3T0Z6Q8M2C4R7V1X5N9PA --plan household --decider acp", "artefact classify 01K9B3T0Z6Q8M2C4R7V1X5N9PA --plan household --node 3.2"},
		Effects:  []string{"Writes classes.<plan> in the card and a classify event with the decider's steps."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				var out []*store.ClassifyResult
				for _, id := range ctx.List("id") {
					var r *store.ClassifyResult
					var err error
					if _, human := ctx.Args["node"]; human {
						r, err = s.SetClass(id, ctx.Str("plan"), ctx.Str("node"), who(ctx))
					} else {
						r, err = s.Classify(context.Background(), id, ctx.Str("plan"), ctx.Str("decider"))
					}
					if err != nil {
						if len(ctx.List("id")) == 1 {
							return nil, err
						}
						r = &store.ClassifyResult{URI: id, Plan: ctx.Str("plan"), Status: "failed", Error: err.Error()}
					}
					out = append(out, r)
				}
				return out, nil
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "plans", Name: "review", Right: "read",
		Summary:    "List the artefacts whose retention has ended, with the rule that applies.",
		Discussion: "With several plans, the longest keep wins. A held artefact (card holds) is marked held. then: archive is applied by --archive (reversible: `artefact unarchive`); then: destroy is never applied here, it goes through `artefact erase`, with its plan and code.",
		Params: []spec.Param{
			{Name: "plan", Kind: spec.String, Help: "Review this plan only."},
			{Name: "due-before", Kind: spec.String, Help: "Count as due what ends before this date (YYYY-MM-DD). Defaults to today."},
			{Name: "archive", Kind: spec.Bool, Help: "Archive the due artefacts whose disposition is archive (not held)."},
		},
		Examples: []string{"artefact review", "artefact review --plan household --due-before 2027-01-01", "artefact review --archive"},
		Effects:  []string{"Reads only; with --archive, marks the due artefacts of disposition archive as archived."},
		Run: func(ctx *spec.Context) (any, error) {
			before := time.Now()
			if d := ctx.Str("due-before"); d != "" {
				t, err := time.Parse("2006-01-02", d)
				if err != nil {
					return nil, spec.UserError("--due-before takes YYYY-MM-DD, got %q", d)
				}
				before = t
			}
			return run(ctx, func(s *store.Store) (any, error) {
				due, err := s.Review(ctx.Str("plan"), before)
				if err != nil || !ctx.Bool("archive") {
					return due, err
				}
				return s.ApplyArchive(due)
			})
		},
	})
}

func count(ns []*classify.Node) int {
	n := 0
	for _, x := range ns {
		n += 1 + count(x.Children)
	}
	return n
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// who names the human behind a CLI decision.
func who(ctx *spec.Context) string {
	if ctx.Access != nil {
		return ctx.Access.Agent
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

func init() {
	for _, v := range []struct {
		name, summary string
		archived      bool
	}{
		{"archive", "Archive artefacts: kept, replicated and reachable by id and view path, but hidden from lists and view listings.", true},
		{"unarchive", "Make archived artefacts active again.", false},
	} {
		v := v
		spec.Register(&spec.Action{
			Category: "artefacts", Name: v.name, Right: "write",
			Summary: v.summary,
			Params: []spec.Param{
				{Name: "id", Kind: spec.StringList, Positional: true, Required: true, Help: "Artefact ids or URIs."},
				{Name: "reason", Kind: spec.String, Help: "Why, kept in the event."},
			},
			Examples: []string{"artefact " + v.name + " 01K9B3T0Z6Q8M2C4R7V1X5N9PA"},
			Effects:  []string{"Sets or clears archived in the card and appends an " + v.name + " event. Nothing is deleted."},
			Run: func(ctx *spec.Context) (any, error) {
				return run(ctx, func(s *store.Store) (any, error) {
					var out []map[string]any
					for _, id := range ctx.List("id") {
						c, err := s.Archive(id, v.archived, ctx.Str("reason"))
						if err != nil {
							return nil, err
						}
						out = append(out, map[string]any{"uri": store.URI(c.Sphere, c.ID), "archived": c.Archived})
					}
					return out, nil
				})
			},
		})
	}
}
