package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/classify"
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
	opened   bool            // its children are shown
	item     *store.ListItem // an artefact leaf
}

func (r treeRow) key() string {
	if r.item != nil {
		return "a:" + r.item.URI
	}
	return r.plan + ":" + r.notation
}

const unclassifiedRow = "_unclassified"

// planData is what the tree is drawn from: a plan, and where each artefact
// of its sphere sits in it. The artefacts themselves are the list's, with its
// filters and search, so that the tree and the list show the same ones.
type planData struct {
	plan    *classify.Plan
	members map[string]string // artefact id → notation; absent or "" when not placed
}

type planView struct {
	data   []planData
	rows   []treeRow
	open   map[string]bool
	sel    int
	top    int
	assign *store.ListItem // nil: browse
	parent *planView       // the browsing tree an assignment came from
	focus  string          // artefact to select once the plans are read
}

type plansMsg struct {
	data []planData
	err  error
}

// loadPlans reads every plan of the granted spheres and where each artefact sits.
func (m *model) loadPlans() tea.Cmd {
	return func() tea.Msg {
		plans, err := m.st.Plans().List()
		if err != nil {
			return plansMsg{err: err}
		}
		var out []planData
		for _, p := range plans {
			if !contains(m.spheres, p.Sphere) {
				continue
			}
			members, err := m.st.ClassMembers(p.Sphere, p.ID)
			if err != nil {
				return plansMsg{err: err}
			}
			d := planData{plan: p, members: map[string]string{}}
			for id, c := range members {
				d.members[id] = c.Node
			}
			out = append(out, d)
		}
		return plansMsg{data: out}
	}
}

// rebuildTree lays out the visible rows from the plans, the list's artefacts
// and the open classes, keeping the selection on the same row when it is
// still shown. While a filter or a search is on, the classes that hold a
// match open by themselves.
func (m *model) rebuildTree() {
	pv := m.planView
	if pv == nil {
		return
	}
	selKey := ""
	if pv.sel < len(pv.rows) {
		selKey = pv.rows[pv.sel].key()
	}
	sphere := m.sphere
	if pv.assign != nil {
		sphere = pv.assign.Sphere
	}
	filtering := m.filter != "" && pv.assign == nil
	visible := m.visible()
	var rows []treeRow
	for _, d := range pv.data {
		p := d.plan
		if sphere != "" && p.Sphere != sphere {
			continue
		}
		items := map[string][]store.ListItem{}
		own := map[string]int{}
		total := 0
		for _, it := range visible {
			if it.Sphere != p.Sphere {
				continue
			}
			total++
			node := d.members[it.ID]
			if node == "" {
				node = unclassifiedRow
			}
			items[node] = append(items[node], it)
			own[node]++
		}
		var count func(n *classify.Node) int
		count = func(n *classify.Node) int {
			c := own[n.Notation]
			for _, k := range n.Children {
				c += count(k)
			}
			return c
		}
		isOpen := func(key string, c int) bool { return pv.open[key] || (filtering && c > 0) }
		head := treeRow{plan: p.ID, sphere: p.Sphere, label: p.Title, count: total - own[unclassifiedRow], kids: true}
		head.opened = isOpen(head.key(), total)
		rows = append(rows, head)
		if !head.opened {
			continue
		}
		var walk func(ns []*classify.Node, leaves []store.ListItem, indent string)
		walk = func(ns []*classify.Node, leaves []store.ListItem, indent string) {
			n := len(ns) + len(leaves)
			for k, node := range ns {
				branch, next := "├── ", "│   "
				if k == n-1 {
					branch, next = "└── ", "    "
				}
				var mine []store.ListItem
				if pv.assign == nil {
					mine = append(mine, items[node.Notation]...)
					store.SortList(mine, m.effectiveOrder(), m.score())
				}
				c := count(node)
				r := treeRow{plan: p.ID, sphere: p.Sphere, notation: node.Notation, label: node.Label,
					count: c, branch: indent + branch, kids: len(node.Children)+len(mine) > 0}
				if filtering && c == 0 {
					continue
				}
				r.opened = r.kids && isOpen(r.key(), c)
				rows = append(rows, r)
				if r.opened {
					walk(node.Children, mine, indent+next)
				}
			}
			for k := range leaves {
				branch := "├── "
				if len(ns)+k == n-1 {
					branch = "└── "
				}
				it := leaves[k]
				rows = append(rows, treeRow{plan: p.ID, sphere: p.Sphere, label: it.Title, branch: indent + branch, item: &it})
			}
		}
		// unclassified closes the plan's tree, as a last top-level class.
		nodes := append(append([]*classify.Node(nil), p.Nodes...), &classify.Node{Notation: unclassifiedRow, Label: "unclassified"})
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

// score gives the relevance of a search result, for the relevance sort.
func (m *model) score() func(store.ListItem) float64 {
	if !m.searched {
		return nil
	}
	return func(it store.ListItem) float64 { return m.scores[it.URI] }
}

// treeItem is the artefact selected in the browsing tree, if any.
func (m *model) treeItem() *store.ListItem {
	pv := m.planView
	if pv == nil || pv.assign != nil || pv.sel >= len(pv.rows) {
		return nil
	}
	return pv.rows[pv.sel].item
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
	pv := &planView{assign: assign, open: m.treeOpen}
	if assign != nil && m.planView != nil && m.planView.assign == nil {
		pv.parent = m.planView
		pv.data = m.planView.data
		m.planView = pv
		m.rebuildTree()
	}
	m.planView = pv
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
			m.rebuildTree()
		}
	}
	if k.String() != "g" {
		m.pendingG = false
	}
	switch k.String() {
	case "esc", "C":
		if m.filter != "" && pv.assign == nil && k.String() == "esc" {
			m.filter, m.searched = "", false
			m.rebuildTree()
			return m.load()
		}
		selected := m.treeItem()
		m.planView = pv.parent
		m.rebuildTree()
		if m.planView == nil && selected != nil {
			m.selectInList(selected.URI) // back to the list, on the same artefact
		}
		return nil
	case "j", "down":
		pv.sel = min(pv.sel+1, max(0, len(pv.rows)-1))
	case "k", "up":
		pv.sel = max(0, pv.sel-1)
	case "g":
		if m.pendingG {
			m.pendingG, pv.sel = false, 0 // gg: top
		} else {
			m.pendingG = true
		}
		return nil
	case "home":
		pv.sel = 0
	case "G", "end":
		pv.sel = max(0, len(pv.rows)-1)
	case "pgdown", "ctrl+d":
		pv.sel = min(pv.sel+m.h/2, max(0, len(pv.rows)-1))
	case "pgup", "ctrl+u":
		pv.sel = max(0, pv.sel-m.h/2)
	case "l", "right":
		switch {
		case r == nil:
		case r.item != nil:
			return m.showDetail(*r.item)
		case !r.opened:
			toggle(true)
		case pv.sel+1 < len(pv.rows):
			pv.sel++
		}
	case "h", "left", "backspace":
		if r == nil {
			return nil
		}
		if r.item == nil && r.opened && pv.open[r.key()] {
			toggle(false)
			return nil
		}
		pv.sel = parentRow(pv.rows, pv.sel)
	case "t", "T":
		o := m.nextOrder()
		if k.String() == "T" {
			o = m.effectiveOrder()
			o.Desc = !o.Desc
		}
		m.order = o
		m.sortAll()
		m.rebuildTree()
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
	default:
		// Every other key does what it does in the list, on the selected artefact.
		if pv.assign == nil {
			return tea.Batch(m.keyList(k), m.refreshCopies())
		}
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
	m.planView = m.planView.parent
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
	var lines []string
	if pv.assign != nil {
		lines = []string{sTitle.Render("Place "+pv.assign.Title) + sMuted.Render("  ·  l opens, h closes  ·  enter places it there  ·  esc back"), ""}
	} else {
		lines = []string{pad(m.header(), m.w), sMuted.Render("  plans  ·  l opens a class, h closes it  ·  enter lists the class, or shows the artefact  ·  every key of the list works here"), ""}
	}
	h := m.h - 3 - len(lines)
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
			if r.opened {
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

// focusTree opens the classes that lead to the artefact uri, in the first
// plan of its sphere, and selects it.
func (m *model) focusTree(uri string) {
	pv := m.planView
	if pv == nil {
		return
	}
	var it *store.ListItem
	for _, x := range m.visible() {
		if x.URI == uri {
			x := x
			it = &x
			break
		}
	}
	if it == nil {
		return
	}
	for _, d := range pv.data {
		if d.plan.Sphere != it.Sphere {
			continue
		}
		pv.open[d.plan.ID+":"] = true
		node := d.members[it.ID]
		if node == "" {
			pv.open[d.plan.ID+":"+unclassifiedRow] = true
		} else {
			var find func(ns []*classify.Node, at []string) []string
			find = func(ns []*classify.Node, at []string) []string {
				for _, n := range ns {
					p := append(append([]string(nil), at...), n.Notation)
					if n.Notation == node {
						return p
					}
					if r := find(n.Children, p); r != nil {
						return r
					}
				}
				return nil
			}
			path := find(d.plan.Nodes, nil)
			for _, n := range path {
				pv.open[d.plan.ID+":"+n] = true
			}
		}
		break
	}
	m.rebuildTree()
	for i, r := range pv.rows {
		if r.item != nil && r.item.URI == uri {
			pv.sel = i
			return
		}
	}
}
