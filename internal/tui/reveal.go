package tui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
	"github.com/aclemen1/artefact-cli/internal/view"
)

// viewPath finds the artefact in a mounted view of its sphere: the Finder's
// WebDAV volume first, then the views.mounts directories. With a plan, it
// is the artefact's place in that plan's tree (plans/<plan>/…), else a/<id>.
func viewPath(cfg *config.Config, st *store.Store, it store.ListItem, plan string) string {
	dirs := []string{filepath.Join("/Volumes", it.Sphere)}
	for _, d := range cfg.Views.Mounts[it.Sphere] {
		dirs = append(dirs, config.Expand(d))
	}
	rels := []string{filepath.Join("a", it.ID+it.Ext)}
	if plan != "" && st != nil {
		if rel, ok := view.New(st, it.Sphere).PlanPath(plan, it.ID); ok {
			rels = append([]string{filepath.FromSlash(rel)}, rels...)
		}
	}
	for _, rel := range rels {
		for _, d := range dirs {
			p := filepath.Join(d, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// reveal shows the artefact's file in the file manager, selected.
// showInFileManager shows the artefact's file; in the plans tree, at its
// place in the plan.
func (m *model) showInFileManager(it store.ListItem) tea.Cmd {
	plan := ""
	if pv := m.planView; pv != nil && pv.assign == nil && pv.sel < len(pv.rows) && m.mode != modeDetail {
		plan = pv.rows[pv.sel].plan
	}
	if it.Sealed {
		m.setStatus("a sealed artefact is in no view: o decrypts it into a temporary file", true)
		return nil
	}
	return func() tea.Msg {
		p, where := viewPath(m.cfg, m.st, it, plan), "view"
		if p == "" {
			var err error
			if p, _, err = m.locate(m.cfg, m.st, it.URI); err != nil {
				return doneMsg{err: err}
			}
			where = "local copy"
		}
		if err := revealInFileManager(p); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: fmt.Sprintf("shown in the file manager, from the %s: %s", where, p)}
	}
}

// revealInFileManager opens the file manager on path's folder with path
// selected: the Finder on macOS; on Linux, the freedesktop FileManager1
// interface (Nautilus, Dolphin, Nemo, Thunar), else the folder with xdg-open.
func revealInFileManager(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Run()
	case "linux":
		u := (&url.URL{Scheme: "file", Path: path}).String()
		err := exec.Command("dbus-send", "--session", "--dest=org.freedesktop.FileManager1", "--type=method_call",
			"/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems",
			"array:string:"+u, "string:").Run()
		if err == nil {
			return nil
		}
		return exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
	return fmt.Errorf("showing a file in the file manager is not supported on %s", runtime.GOOS)
}
