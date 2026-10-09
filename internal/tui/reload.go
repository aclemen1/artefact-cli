package tui

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/config"
)

// The TUI reloads itself when its binary changes (a rebuild): at rest, it
// execs the new binary and carries its state over; while the user types or
// edits, it shows a badge and waits. SIGUSR1 asks for the same.

const resumeEnv = "ARTEFACT_TUI_RESUME"

type binStamp struct {
	mod time.Time
	ino uint64
}

func stampOf(path string) (binStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return binStamp{}, false
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return binStamp{fi.ModTime(), ino}, true
}

// buildLabel is the version, with the short commit the binary was built
// from (+ when the working copy had changes), else its build time.
func buildLabel(version, exe string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, modified := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if len(rev) >= 8 {
			rev = rev[:8]
			if modified {
				rev += "+"
			}
			return version + " " + rev
		}
	}
	if st, ok := stampOf(exe); ok {
		return version + " " + st.mod.Format("02.01 15:04")
	}
	return version
}

// resume is the state carried over a reload.
type resume struct {
	Detail   string    `json:"detail,omitempty"`   // artefact shown in the detail view
	Selected string    `json:"selected,omitempty"` // artefact selected in the list or the tree
	Filter   string    `json:"filter,omitempty"`
	Searched bool      `json:"searched,omitempty"`
	Archived bool      `json:"archived,omitempty"`
	Pending  bool      `json:"pending,omitempty"`
	Orphaned bool      `json:"orphaned,omitempty"`
	Plans    bool      `json:"plans,omitempty"`
	Class    *classRef `json:"class,omitempty"` // the list filtered on a class
}

type classRef struct {
	Plan, Sphere, Notation, Label string
}

type reloadMsg struct{} // SIGUSR1

// idle reports whether nothing the user is doing would be lost by a reload.
func (m *model) idle() bool {
	return !m.modal.Open() && m.prompt == promptNone && m.editCard == nil && m.toSave == nil && len(m.edits) == 0 &&
		m.searching == "" && (m.planView == nil || m.planView.assign == nil)
}

// checkBinary notes a new binary, and reloads when the TUI is at rest.
func (m *model) checkBinary() tea.Cmd {
	if m.exe == "" {
		return nil
	}
	if st, ok := stampOf(m.exe); ok && st != m.exeStamp {
		m.newBinary = true
	}
	return m.reloadIfIdle()
}

func (m *model) reloadIfIdle() tea.Cmd {
	if !m.newBinary || !m.idle() {
		return nil
	}
	if err := m.saveResume(); err != nil {
		m.setStatus("cannot reload: "+err.Error(), true)
		m.newBinary = false
		return nil
	}
	m.reexec = true
	return tea.Quit
}

// resumePath is where a reload leaves the state for the new process.
var resumePath = func() string { return config.Expand("~/.local/state/artefact/tui-resume.json") }

func (m *model) saveResume() error {
	r := resume{Filter: m.filter, Searched: m.searched, Archived: m.archived, Pending: m.pending,
		Orphaned: m.orphaned, Plans: m.planView != nil}
	if cf := m.classFilter; cf.plan != "" {
		r.Class = &classRef{cf.plan, cf.sphere, cf.notation, cf.label}
	}
	if m.mode == modeDetail && m.detail != nil {
		r.Detail = m.detail.URI
	}
	if it := m.current(); it != nil {
		r.Selected = it.URI
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := resumePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// loadResume applies the state a reload carried over, once.
func (m *model) loadResume() {
	p := os.Getenv(resumeEnv)
	if p == "" {
		return
	}
	os.Unsetenv(resumeEnv)
	b, err := os.ReadFile(p)
	os.Remove(p)
	if err != nil {
		return
	}
	var r resume
	if json.Unmarshal(b, &r) != nil {
		return
	}
	m.resumed = &r
	m.filter, m.archived, m.pending, m.orphaned = r.Filter, r.Archived, r.Pending, r.Orphaned
	if c := r.Class; c != nil {
		m.classFilter = treeRow{plan: c.Plan, sphere: c.Sphere, notation: c.Notation, label: c.Label}
	}
	m.startInPlans = r.Plans
	m.setStatus("reloaded "+m.build, false)
}

// resumeCmds restores, once the list is read, what the reload carried over.
func (m *model) resumeCmds() tea.Cmd {
	r := m.resumed
	if r == nil {
		return nil
	}
	m.resumed = nil
	var cmds []tea.Cmd
	if r.Searched && r.Filter != "" {
		sphere, state := m.sphere, "active"
		if m.archived {
			state = "archived"
		}
		cmds = append(cmds, m.startSearch(r.Filter, sphere, state))
		m.setStatus("reloaded "+m.build, false)
	}
	if r.Selected != "" {
		switch {
		case m.planView != nil && m.planView.data != nil:
			m.focusTree(r.Selected)
		case m.planView != nil:
			m.planView.focus = r.Selected // the plans are still being read
		default:
			m.selectInList(r.Selected)
		}
	}
	if r.Detail != "" {
		m.mode, m.detail, m.scroll = modeDetail, nil, 0
		cmds = append(cmds, m.loadDetail(r.Detail))
	}
	return tea.Batch(cmds...)
}

// watchSignals turns SIGUSR1 into a reload request.
func watchSignals(p *tea.Program) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			p.Send(reloadMsg{})
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}

// execSelf replaces the process with the binary at exe, carrying the state.
func execSelf(exe string) error {
	env := append(os.Environ(), resumeEnv+"="+resumePath())
	return syscall.Exec(exe, os.Args, env)
}
