package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// The plans view shows the classification plans as a tree, with how many
// artefacts each class holds (sub-classes included). It serves two uses:
// browse (enter filters the list on a class) and assign (enter places the
// chosen artefact in a class, as a human decision).

type treeRow struct {
	plan     string
	sphere   string
	notation string // "" for the plan itself; unclassified for the unplaced
	label    string
	depth    int
	count    int
}

const unclassifiedRow = "_unclassified"

type planView struct {
	rows   []treeRow
	sel    int
	top    int
	assign *store.ListItem // nil: browse
}

type plansMsg struct {
	rows []treeRow
	err  error
}

// loadPlans builds the tree of the plans of the spheres in view.
func (m *model) loadPlans() tea.Cmd {
	sphere := m.sphere
	if m.planView != nil && m.planView.assign != nil {
		sphere = m.planView.assign.Sphere
	}
	return func() tea.Msg {
		plans, err := m.st.Plans().List()
		if err != nil {
			return plansMsg{err: err}
		}
		var rows []treeRow
		for _, p := range plans {
			if sphere != "" && p.Sphere != sphere {
				continue
			}
			if !contains(m.spheres, p.Sphere) {
				continue
			}
			members, err := m.st.ClassMembers(p.Sphere, p.ID)
			if err != nil {
				return plansMsg{err: err}
			}
			own := map[string]int{}
			placed := 0
			for _, c := range members {
				if c.Node != "" {
					own[c.Node]++
					placed++
				}
			}
			total := 0
			if items, err := m.st.List(p.Sphere, indexFilterAll()); err == nil {
				total = len(items)
			}
			rows = append(rows, treeRow{plan: p.ID, sphere: p.Sphere, label: p.Title, count: placed})
			var walk func(ns []*classify.Node, d int) int
			walk = func(ns []*classify.Node, d int) int {
				sum := 0
				for _, n := range ns {
					i := len(rows)
					rows = append(rows, treeRow{plan: p.ID, sphere: p.Sphere, notation: n.Notation, label: n.Label, depth: d})
					c := own[n.Notation] + walk(n.Children, d+1)
					rows[i].count = c
					sum += c
				}
				return sum
			}
			walk(p.Nodes, 1)
			rows = append(rows, treeRow{plan: p.ID, sphere: p.Sphere, notation: unclassifiedRow, label: "unclassified", depth: 1, count: total - placed})
		}
		return plansMsg{rows: rows}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// openPlans shows the tree; assign names the artefact to place, or nil.
func (m *model) openPlans(assign *store.ListItem) tea.Cmd {
	m.planView = &planView{assign: assign}
	return m.loadPlans()
}

func (m *model) keyPlans(k tea.KeyPressMsg) tea.Cmd {
	pv := m.planView
	switch k.String() {
	case "esc", "q", "h", "left", "C":
		m.planView = nil
		return nil
	case "j", "down":
		pv.sel = min(pv.sel+1, max(0, len(pv.rows)-1))
	case "k", "up":
		pv.sel = max(0, pv.sel-1)
	case "g", "home":
		pv.sel = 0
	case "G", "end":
		pv.sel = max(0, len(pv.rows)-1)
	case "enter", "l", "right":
		if pv.sel >= len(pv.rows) {
			return nil
		}
		r := pv.rows[pv.sel]
		if pv.assign != nil {
			return m.assignClass(*pv.assign, r)
		}
		m.planView = nil
		m.classFilter, m.sel, m.filter, m.searched = r, 0, "", false
		return m.load()
	}
	return nil
}

// assignClass records the human decision for an artefact.
func (m *model) assignClass(it store.ListItem, r treeRow) tea.Cmd {
	node := r.notation
	if node == unclassifiedRow {
		node = ""
	}
	if r.notation == "" {
		m.setStatus("pick a class of the plan, or unclassified", true)
		return nil
	}
	m.planView = nil
	return func() tea.Msg {
		res, err := m.st.SetClass(it.URI, r.plan, node, who())
		if err != nil {
			return doneMsg{err: err}
		}
		if res.Status == "removed" {
			return doneMsg{status: "class removed from " + r.plan, reload: true}
		}
		return doneMsg{status: fmt.Sprintf("placed in %s %s (%s)", res.Node, res.Label, r.plan), reload: true}
	}
}

func (m *model) renderPlans() string {
	pv := m.planView
	title := "Plans"
	if pv.assign != nil {
		title = "Place " + pv.assign.Title
	}
	lines := []string{sTitle.Render(title) + sMuted.Render("  ·  enter "+map[bool]string{true: "places it there", false: "lists the class"}[pv.assign != nil]+"  ·  esc back"), ""}
	h := m.h - 4
	if pv.sel < pv.top {
		pv.top = pv.sel
	}
	if pv.sel >= pv.top+h {
		pv.top = pv.sel - h + 1
	}
	if len(pv.rows) == 0 {
		lines = append(lines, sMuted.Render("  no plan for this sphere; add one with `artefact plan add <file.yaml>`"))
	}
	for i := pv.top; i < len(pv.rows) && i < pv.top+h; i++ {
		r := pv.rows[i]
		var text string
		switch {
		case r.notation == "":
			text = sBold.Render(r.label) + sMuted.Render("  "+r.plan+" · ") + tag(r.sphere)
		case r.notation == unclassifiedRow:
			text = strings.Repeat("  ", r.depth) + sWarn.Render("unclassified")
		default:
			text = strings.Repeat("  ", r.depth) + sMuted.Render(r.notation) + " " + sText.Render(r.label)
		}
		row := fmt.Sprintf("%-*s", 0, text)
		row = pad(row, m.w-8) + sMuted.Render(fmt.Sprintf("%6d", r.count))
		if i == pv.sel {
			row = selectLine(pad(row, m.w), m.w)
		}
		lines = append(lines, row)
	}
	return block(lines, m.w, m.h-2)
}
