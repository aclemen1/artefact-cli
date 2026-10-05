package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// prefs is what the TUI keeps from one session to the next.
type prefs struct {
	Sort   string   `json:"sort,omitempty"`   // key:asc|desc; "" for the default
	Sphere string   `json:"sphere,omitempty"` // "" for every sphere
	Open   []string `json:"open,omitempty"`   // classes open in the plans tree
}

func prefsPath() string { return config.Expand("~/.local/state/artefact/tui.json") }

func (m *model) currentPrefs() prefs {
	p := prefs{Sphere: m.sphere}
	if m.order.Key != "" {
		p.Sort = m.order.String()
	}
	for k, open := range m.treeOpen {
		if open {
			p.Open = append(p.Open, k)
		}
	}
	slices.Sort(p.Open)
	return p
}

// loadPrefs applies the saved preferences; a missing or unreadable file
// leaves the defaults.
func (m *model) loadPrefs(path string) {
	m.prefsFile = path
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var p prefs
	if json.Unmarshal(b, &p) != nil {
		return
	}
	if o, err := store.ParseSort(p.Sort); err == nil {
		m.order = o
	}
	if p.Sphere == "" || slices.Contains(m.spheres, p.Sphere) {
		m.sphere = p.Sphere
	}
	for _, k := range p.Open {
		m.treeOpen[k] = true
	}
	m.saved = m.currentPrefs()
}

// savePrefs writes the preferences when they changed.
func (m *model) savePrefs() {
	if m.prefsFile == "" {
		return
	}
	p := m.currentPrefs()
	if slices.Equal(p.Open, m.saved.Open) && p.Sort == m.saved.Sort && p.Sphere == m.saved.Sphere {
		return
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if os.MkdirAll(filepath.Dir(m.prefsFile), 0o700) != nil {
		return
	}
	tmp := m.prefsFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, m.prefsFile) == nil {
		m.saved = p
	}
}

// Update handles a message, then keeps the preferences a key may have changed.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mm, cmd := m.update(msg)
	if _, ok := msg.(tea.KeyPressMsg); ok {
		m.savePrefs()
	}
	return mm, cmd
}
