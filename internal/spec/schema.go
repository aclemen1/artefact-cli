package spec

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type CategoryEntry struct {
	Category    string `json:"category"`
	ActionCount int    `json:"action_count"`
}

type ActionEntry struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

func Catalog() []CategoryEntry {
	counts := map[string]int{}
	for _, a := range registry {
		counts[a.Category]++
	}
	var out []CategoryEntry
	for _, c := range Categories() {
		out = append(out, CategoryEntry{c, counts[c]})
	}
	return out
}

func ActionsIn(category string) []ActionEntry {
	var out []ActionEntry
	for _, a := range registry {
		if a.Category == category {
			out = append(out, ActionEntry{a.Name, a.Summary})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type Leaf struct {
	*Action
	Usage string `json:"usage"`
}

func Search(q string) []ActionEntry {
	q = strings.ToLower(q)
	var out []ActionEntry
	for _, a := range registry {
		hay := strings.ToLower(a.Category + " " + a.Name + " " + a.Summary + " " + a.Discussion)
		if strings.Contains(hay, q) {
			out = append(out, ActionEntry{a.Category + " " + a.Name, a.Summary})
		}
	}
	return out
}

func TextSchema(w io.Writer, v any) {
	switch r := v.(type) {
	case []CategoryEntry:
		for _, c := range r {
			fmt.Fprintf(w, "%-10s %d actions\n", c.Category, c.ActionCount)
		}
	case []ActionEntry:
		for _, a := range r {
			fmt.Fprintf(w, "%-16s %s\n", a.Name, a.Summary)
		}
	case Leaf:
		fmt.Fprintf(w, "%s\n\n%s\n", r.Usage, r.Summary)
		if r.Discussion != "" {
			fmt.Fprintf(w, "\n%s\n", r.Discussion)
		}
		if len(r.Params) > 0 {
			fmt.Fprintln(w, "\nParameters:")
			for _, p := range r.Params {
				extra := ""
				if len(p.Enum) > 0 {
					extra = " (" + strings.Join(p.Enum, "|") + ")"
				}
				if p.Default != "" {
					extra += " default " + p.Default
				}
				fmt.Fprintf(w, "  %-20s %s%s\n", describeParam(p), p.Help, extra)
			}
		}
		if len(r.Effects) > 0 {
			fmt.Fprintln(w, "\nEffects:")
			for _, e := range r.Effects {
				fmt.Fprintf(w, "  - %s\n", e)
			}
		}
		if len(r.Examples) > 0 {
			fmt.Fprintln(w, "\nExamples:")
			for _, e := range r.Examples {
				fmt.Fprintf(w, "  %s\n", e)
			}
		}
	}
}
