package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/classify"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// The plans view shows the classification plans as a tree, with how many
// artefacts each class holds (sub-classes included); a class opens on its
// sub-classes and its artefacts. It serves two uses: browse, and assign
// (enter places the chosen artefact in a class, as a human decision; the
// tree then shows classes only).

type treeRow struct {
	plan     string
	sphere   string
	notation string // "" for the plan itself; unclassified for the unplaced
	label    string
	count    int
	branch   string          // tree lines before the label: "│   ├── "
	kids     bool            // the row can open
	item     *store.ListItem // an artefact leaf
}

func (r treeRow) key() string {
	if r.item != nil {
		return "a:" + r.item.URI
	}
	return r.plan + ":" + r.notation
}

const unclassifiedRow = "_unclassified"

// planData is what the tree is drawn from.
type planData struct {
	plan  *classify.Plan
	items map[string][]store.ListItem // by node; "" for the unplaced
	own   map[string]int
	total int
}

type planView struct {
	data   []planData
	rows   []treeRow
	open   map[string]bool
	sel    int
	top    int
	assign *store.ListItem // nil: browse
}

type plansMsg struct {
	data []planData
	err  error
}

// loadPlans reads the plans of the spheres in view and where each artefact sits.
func (m *model) loadPlans() tea.Cmd {
	sphere := m.sphere
	if m.planView != nil && m.planView.assign != nil {
		sphere = m.planView.assign.Sphere
	}
	state := "active"
	if m.archived {
		state = "archived"
	}
	return func() tea.Msg {
		plans, err := m.st.Plans().List()
		if err != nil {
			return plansMsg{err: err}
		}
		var out []planData
		for _, p := range plans {
			if (sphere != "" && p.Sphere != sphere) || !contains(m.spheres, p.Sphere) {
				continue
			}
			members, err := m.st.ClassMembers(p.Sphere, p.ID)
			if err != nil {
				return plansMsg{err: err}
			}
			items, err := m.st.List(p.Sphere, index.Filter{State: state})
			if err != nil {
				return plansMsg{err: err}
			}
			d := planData{plan: p, items: map[string][]store.ListItem{}, own: map[string]int{}, total: len(items)}
			for _, it := range items {
				node := members[it.ID].Node
				d.items[node] = append(d.items[node], it)
				if node != "" {
					d.own[node]++
				}
			}
			out = append(out, d)
		}
		return plansMsg{data: out}
	}
}

// rebuild lays out the visible rows from the data and the open classes,
// keeping the selection on the same row when it is still shown.
func (pv *planView) rebuild(order store.Sort) {
	selKey := ""
	if pv.sel < len(pv.rows) {
		selKey = pv.rows[pv.sel].key()
	}
	var rows []treeRow
	leaves := func(d planData, node string) []store.ListItem {
		if pv.assign != nil {
			return nil
		}
		l := append([]store.ListItem(nil), d.items[node]...)
		store.SortList(l, order, nil)
		return l
	}
	var count func(d planData, n *classify.Node) int
	count = func(d planData, n *classify.Node) int {
		c := d.own[n.Notation]
		for _, k := range n.Children {
			c += count(d, k)
		}
		return c
	}
	for _, d := range pv.data {
		p := d.plan
		placed := d.total - len(d.items[""])
		head := treeRow{plan: p.ID, sphere: p.Sphere, label: p.Title, count: placed, kids: true}
		rows = append(rows, head)
		if !pv.open[head.key()] {
			continue
		}
		var walk func(ns []*classify.Node, items []store.ListItem, indent string)
		walk = func(ns []*classify.Node, items []store.ListItem, indent string) {
			n := len(ns) + len(items)
			for k, node := range ns {
				branch, next := "├── ", "│   "
				if k == n-1 {
					branch, next = "└── ", "    "
				}
				own := leaves(d, node.Notation)
				r := treeRow{plan: p.ID, sphere: p.Sphere, notation: node.Notation, label: node.Label,
					count: count(d, node), branch: indent + branch, kids: len(node.Children)+len(own) > 0}
				rows = append(rows, r)
				if pv.open[r.key()] {
					walk(node.Children, own, indent+next)
				}
			}
			for k := range items {
				branch := "├── "
				if len(ns)+k == n-1 {
					branch = "└── "
				}
				it := items[k]
				rows = append(rows, treeRow{plan: p.ID, sphere: p.Sphere, label: it.Title, branch: indent + branch, item: &it})
			}
		}
		// unclassified closes the plan's tree, as a last top-level class.
		nodes := append(append([]*classify.Node(nil), p.Nodes...), &classify.Node{Notation: unclassifiedRow, Label: "unclassified"})
		d.own[unclassifiedRow] = len(d.items[""])
		d.items[unclassifiedRow] = d.items[""]
		walk(nodes, nil, "")
	}
	pv.rows = rows
	pv.sel = min(pv.sel, max(0, len(rows)-1))
	for i, r := range rows {
		if r.key() == selKey {
			pv.sel = i
			break
		}
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
	m.planView = &planView{assign: assign, open: m.treeOpen}
	return m.loadPlans()
}

func (m *model) keyPlans(k tea.KeyPressMsg) tea.Cmd {
	pv := m.planView
	m.status = ""
	var r *treeRow
	if pv.sel < len(pv.rows) {
		r = &pv.rows[pv.sel]
	}
	toggle := func(open bool) {
		if r != nil && r.kids {
			pv.open[r.key()] = open
			pv.rebuild(m.effectiveOrder())
		}
	}
	switch k.String() {
	case "esc", "q", "C":
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
	case "pgdown", "ctrl+d":
		pv.sel = min(pv.sel+m.h/2, max(0, len(pv.rows)-1))
	case "pgup", "ctrl+u":
		pv.sel = max(0, pv.sel-m.h/2)
	case "space", "tab":
		if r != nil {
			toggle(!pv.open[r.key()])
		}
	case "l", "right":
		switch {
		case r == nil:
		case r.item != nil:
			return m.showDetail(*r.item)
		case !pv.open[r.key()]:
			toggle(true)
		case pv.sel+1 < len(pv.rows):
			pv.sel++
		}
	case "h", "left", "backspace":
		if r == nil {
			return nil
		}
		if r.item == nil && pv.open[r.key()] {
			toggle(false)
			return nil
		}
		pv.sel = parentRow(pv.rows, pv.sel)
	case "o":
		if r != nil && r.item != nil {
			return m.open(r.item.URI, r.item.Mime)
		}
	case "t", "T":
		o := m.nextOrder()
		if k.String() == "T" {
			o = m.effectiveOrder()
			o.Desc = !o.Desc
		}
		m.order = o
		m.sortAll()
		pv.rebuild(m.effectiveOrder())
		m.setStatus("artefacts sorted by "+m.orderLabel(), false)
	case "enter":
		if r == nil {
			return nil
		}
		if r.item != nil {
			return m.showDetail(*r.item)
		}
		if pv.assign != nil {
			return m.assignClass(*pv.assign, *r)
		}
		m.planView = nil
		m.classFilter, m.sel, m.filter, m.searched = *r, 0, "", false
		return m.load()
	}
	return nil
}

// parentRow is the row whose branch is one level shallower above i.
func parentRow(rows []treeRow, i int) int {
	depth := len([]rune(rows[i].branch))
	for j := i - 1; j >= 0; j-- {
		if len([]rune(rows[j].branch)) < depth {
			return j
		}
	}
	return i
}

func (m *model) showDetail(it store.ListItem) tea.Cmd {
	m.mode, m.detail, m.scroll = modeDetail, nil, 0
	return m.loadDetail(it.URI)
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
	hint := "  ·  space opens  ·  enter lists the class, or shows the artefact  ·  o opens it  ·  t/T sort: " + m.orderLabel() + "  ·  esc back"
	if pv.assign != nil {
		title = "Place " + pv.assign.Title
		hint = "  ·  space opens  ·  enter places it there  ·  esc back"
	}
	lines := []string{sTitle.Render(title) + sMuted.Render(hint), ""}
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
		fold := "  "
		if r.kids {
			fold = "▸ "
			if pv.open[r.key()] {
				fold = "▾ "
			}
		}
		var text, right string
		switch {
		case r.item != nil:
			lock := " "
			if r.item.Sealed {
				lock = sWarn.Render("◆")
			}
			text = "  " + sMuted.Render(r.branch) + lock + " " + sText.Render(r.label)
			right = sMuted.Render(fmt.Sprintf("%19s", r.item.Date+"   "+shortID(r.item.ID)))
		case r.notation == "":
			text = fold + sBold.Render(r.label) + sMuted.Render("  "+r.plan+" · ") + tag(r.sphere)
			right = sMuted.Render(fmt.Sprintf("%19d", r.count))
		case r.notation == unclassifiedRow:
			text = "  " + sMuted.Render(r.branch) + fold + sWarn.Render("unclassified")
			right = sMuted.Render(fmt.Sprintf("%19d", r.count))
		default:
			text = "  " + sMuted.Render(r.branch) + fold + sMuted.Render(r.notation) + " " + sText.Render(r.label)
			right = sMuted.Render(fmt.Sprintf("%19d", r.count))
		}
		row := pad(text, m.w-22) + pad(right, 20)
		if i == pv.sel {
			row = selectLine(pad(row, m.w), m.w)
		}
		lines = append(lines, row)
	}
	return block(lines, m.w, m.h-2)
}
