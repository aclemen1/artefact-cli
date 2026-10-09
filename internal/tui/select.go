package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/store"
)

// selectReq is the artefact --select asks the TUI to open on.
type selectReq struct {
	uri      string
	archived bool
	widened  bool // the view was widened once to show it
}

// selectOnStart resolves --select; an unknown id leaves the TUI as usual.
func (m *model) selectOnStart(id string) {
	if id = strings.TrimSpace(id); id == "" {
		return
	}
	r, err := m.st.Resolve(id)
	if err != nil {
		m.setStatus("no artefact "+id+": "+err.Error(), true)
		return
	}
	m.selecting = &selectReq{uri: store.URI(r.Sphere, r.Row.ID), archived: r.Row.Archived != ""}
}

// applySelect selects the artefact once the list is read; when the view
// hides it, the view widens once (every sphere, no filter, archives if
// archived) and the list is read again.
func (m *model) applySelect() tea.Cmd {
	req := m.selecting
	for i, it := range m.visible() {
		if it.URI != req.uri {
			continue
		}
		m.selecting = nil
		if m.planView != nil {
			if m.planView.data != nil {
				m.focusTree(req.uri)
			} else {
				m.planView.focus = req.uri
			}
			return nil
		}
		m.sel = i
		return nil
	}
	if req.widened {
		m.selecting = nil
		m.setStatus("the artefact is not in this view", true)
		return nil
	}
	req.widened = true
	m.sphere, m.pending, m.orphaned, m.archived = "", false, false, req.archived
	m.filter, m.searched, m.classFilter = "", false, treeRow{}
	return m.load()
}

// argsWithoutSelect drops --select from a command line: a reload keeps the
// selection the user has moved to, not the one asked at start.
func argsWithoutSelect(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--select" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--select=") {
			continue
		}
		out = append(out, a)
	}
	return out
}
