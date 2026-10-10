package tui

import (
	"context"
	"fmt"
	"github.com/aclemen1/artefact-cli/internal/notes"
	"github.com/aclemen1/tuikit"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/store"
)

// The keys follow the common convention of the ecosystem's TUIs: o open,
// f Finder, E edit the card, M edit the content, m place in a class,
// e or x archive (reversible), # erase, S seal, c deposit, R sync now,
// 1–5 views. Older keys stay as aliases when they clash with nothing.

// itemKey runs a key that acts on one artefact; ok is false for other keys.
func (m *model) itemKey(key string, it store.ListItem, inTree bool) (tea.Cmd, bool) {
	switch key {
	case "o":
		return m.open(it.URI, it.Mime), true
	case "f":
		return m.showInFileManager(it), true
	case "E", "M":
		st, _, err := m.st.Stat([]string{it.URI}, false)
		if err != nil || len(st) == 0 {
			m.setStatus("cannot read the card", true)
			return nil, true
		}
		if key == "E" {
			return m.edit(st[0].Card), true
		}
		return m.editContent(st[0].Card), true
	case "m":
		cp := it
		return m.openPlans(&cp), true
	case "N":
		if !notes.Configured(m.cfg) || len(m.cfg.Notes.Add) == 0 {
			m.setStatus("no notes.add command in the configuration", true)
			return nil, true
		}
		m.noteFor = &it
		return m.openModal("Note on "+it.Title, tuikit.NewEditor("note", "Note", "")), true
	case "e", "x", "a":
		toArchive := it.Archived == ""
		id := it.URI
		return m.job(archiveLabel(toArchive, it.Title), func() doneMsg {
			if _, err := m.st.Archive(id, toArchive, "tui"); err != nil {
				return doneMsg{err: err}
			}
			if toArchive {
				return doneMsg{status: "archived (x or e again makes it active)", reload: true}
			}
			return doneMsg{status: "active again", reload: true}
		}), true
	case "S":
		if it.Sealed {
			m.setStatus("already sealed", false)
			return nil, true
		}
		m.target = it.URI
		return m.confirm(promptSeal, "seal", "Seal", "Seal "+it.Title+" with the sphere's key, and erase its clear versions everywhere?"), true
	case "#", "X":
		force := key == "X"
		m.force = force
		id := it.URI
		return m.wrap("plan the erasure of "+it.Title, func() tea.Msg {
			p, err := m.st.PlanErase(store.EraseRequest{Refs: []string{id}, Force: force})
			return planMsg{p, err}
		}), true
	}
	return nil, false
}

// goView switches view: 1 the list, 2 the plans, 3 pending, 4 archives,
// 5 orphaned.
func (m *model) goView(key string) tea.Cmd {
	focus := ""
	if it := m.current(); it != nil {
		focus = it.URI
	}
	m.planView, m.mode, m.detail = nil, modeList, nil
	m.pending, m.archived, m.orphaned = false, false, false
	m.sel = 0
	switch key {
	case "2":
		cmd := m.openPlans(nil)
		m.planView.focus = focus
		return tea.Batch(m.load(), cmd)
	case "3":
		m.pending = true
	case "4":
		m.archived = true
	case "5":
		m.orphaned = true
	}
	return m.load()
}

// syncNow copies to the backends what is not there yet, now.
func (m *model) syncNow() tea.Cmd {
	sphere := m.sphere
	return m.job("sync"+sphereSuffix(sphere), func() doneMsg {
		res, err := m.st.Sync(context.Background(), sphere, false)
		if err != nil {
			return doneMsg{err: err}
		}
		sent, errs := 0, 0
		for _, sp := range res {
			for _, b := range sp.Backends {
				sent += b.Uploaded
				errs += len(b.Errors)
			}
		}
		return doneMsg{status: fmt.Sprintf("synced: %d file(s) sent, %d error(s)", sent, errs)}
	})
}
