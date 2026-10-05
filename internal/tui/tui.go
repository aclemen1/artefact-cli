// Package tui is artefact's terminal interface (SPEC §13): the artefacts of
// the granted spheres, their card, versions, holders and events, and the
// actions open, edit card, seal and erase (plan, then code).
package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// Locator gives a local path to open an artefact (the server's Locate).
type Locator func(cfg *config.Config, st *store.Store, id string) (string, string, error)

// Run shows the TUI over a store already scoped to the spheres it may use.
func Run(cfg *config.Config, st *store.Store, locate Locator) error {
	m := newModel(cfg, st, locate)
	defer m.Cleanup()
	_, err := tea.NewProgram(m).Run()
	return err
}

type mode int

const (
	modeList mode = iota
	modeDetail
)

// prompt is a question asked in the status line.
type prompt int

const (
	promptNone prompt = iota
	promptFilter
	promptSeal
	promptErase
	promptReveal
	promptRevise
)

type model struct {
	cfg    *config.Config
	st     *store.Store
	locate Locator

	spheres  []string
	sphere   string // "" for every sphere
	orphaned bool
	pending  bool
	archived bool
	all      []store.ListItem
	sel      int
	top      int

	mode   mode
	detail *store.Stat
	scroll int

	prompt   prompt
	input    textinput.Model
	filter   string
	searched bool               // m.all holds search results: no local filtering
	order    store.Sort         // "" key: deposit date for a list, relevance for a search
	scores   map[string]float64 // relevance of the search results, by URI
	plan     *store.Plan
	force    bool

	copies      map[string]store.CopyState
	progress    []store.SyncProgress
	legend      bool
	legendAt    int
	reveal      *revealReq       // sealed artefact waiting for the confirmation to open it
	edits       map[string]*edit // content being edited in an application, by artefact
	toSave      *edit            // edited copy waiting for the confirmation to save it
	editCard    *store.Card      // card open in the editor
	planView    *planView        // the plans tree, when shown
	classFilter treeRow          // list filtered on a class (plan "" for none)
	revealed    []string         // decrypted temporary files, deleted on quit

	w, h      int
	status    string
	statusErr bool
}

func newModel(cfg *config.Config, st *store.Store, locate Locator) *model {
	in := textinput.New()
	// A steady cursor: the prompts are short-lived and blinking only redraws.
	styles := in.Styles()
	styles.Cursor.Blink = false
	in.SetStyles(styles)
	return &model{cfg: cfg, st: st, locate: locate, spheres: st.Spheres, input: in, w: 100, h: 30, copies: map[string]store.CopyState{}, edits: map[string]*edit{}}
}

// ------------------------------------------------------------------ data

type (
	listMsg struct {
		items []store.ListItem
		err   error
	}
	detailMsg struct {
		st  *store.Stat
		err error
	}
	doneMsg struct {
		status string
		err    error
		reload bool
		back   bool
	}
	planMsg struct {
		plan *store.Plan
		err  error
	}
	searchMsg struct {
		items  []store.ListItem
		scores map[string]float64
	}
	tickMsg   struct{}
	copiesMsg struct {
		states   map[string]store.CopyState
		progress []store.SyncProgress
	}
	editedMsg struct {
		id, path string
		err      error
	}
)

func (m *model) load() tea.Cmd {
	sphere, orphaned, pending := m.sphere, m.orphaned, m.pending
	state := "active"
	if m.archived {
		state = "archived"
	}
	f := index.Filter{Orphaned: orphaned, Pending: pending, State: state}
	if cf := m.classFilter; cf.plan != "" {
		sphere = cf.sphere
		switch cf.notation {
		case "":
			f.Class = cf.plan
		case unclassifiedRow:
			f.Unclassified = cf.plan
		default:
			f.Class = cf.plan + ":" + cf.notation
		}
	}
	return func() tea.Msg {
		items, err := m.st.List(sphere, f)
		return listMsg{items, err}
	}
}

func (m *model) loadDetail(id string) tea.Cmd {
	return func() tea.Msg {
		st, missing, err := m.st.Stat([]string{id}, true)
		if err == nil && len(missing) > 0 {
			err = fmt.Errorf("artefact %s is gone", id)
		}
		if err != nil {
			return detailMsg{err: err}
		}
		return detailMsg{st: &st[0]}
	}
}

// visible is the list once the text filter applies.
func (m *model) visible() []store.ListItem {
	if m.filter == "" || m.searched {
		return m.all
	}
	f := strings.ToLower(m.filter)
	var out []store.ListItem
	for _, it := range m.all {
		if strings.Contains(strings.ToLower(it.Title+" "+it.Filename+" "+it.ID+" "+it.Mime), f) {
			out = append(out, it)
		}
	}
	return out
}

func (m *model) current() *store.ListItem {
	l := m.visible()
	if m.sel < 0 || m.sel >= len(l) {
		return nil
	}
	return &l[m.sel]
}

func (m *model) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

// ------------------------------------------------------------------ update

func (m *model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.load(), tick())
}

const refreshEvery = 3 * time.Second

func tick() tea.Cmd { return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} }) }

// refreshCopies reads the sync progress and the copy state of the rows on
// screen: cheap enough to run every few seconds.
func (m *model) refreshCopies() tea.Cmd {
	l := m.visible()
	h := m.listHeight()
	end := min(len(l), m.top+h)
	start := min(m.top, end)
	rows := append([]store.ListItem{}, l[start:end]...)
	return func() tea.Msg {
		return copiesMsg{states: m.st.CopyStates(rows), progress: m.st.Progress()}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that reports no size still gets a usable layout.
		m.w, m.h = msg.Width, msg.Height
		if m.w < 20 || m.h < 6 {
			m.w, m.h = max(m.w, 80), max(m.h, 24)
		}
		m.input.SetWidth(max(10, m.w-30))
		return m, nil
	case tea.BackgroundColorMsg:
		darkBackground = msg.IsDark()
		return m, nil
	case listMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.all, m.searched = msg.items, false
		m.sortAll()
		m.sel = min(m.sel, max(0, len(m.visible())-1))
		return m, m.refreshCopies()
	case plansMsg:
		if m.planView != nil {
			if msg.err != nil {
				m.setStatus(msg.err.Error(), true)
			}
			m.planView.rows = msg.rows
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.refreshCopies(), m.checkEdits(), tick())
	case editedContentMsg:
		e := m.edits[msg.id]
		if e == nil {
			return m, nil
		}
		if msg.err != nil {
			m.setStatus("editor: "+msg.err.Error(), true)
			return m, nil
		}
		return m, m.askRevise(e)
	case copiesMsg:
		for k, v := range msg.states {
			m.copies[k] = v
		}
		m.progress = msg.progress
		return m, nil
	case searchMsg:
		m.all, m.searched, m.sel, m.scores = msg.items, true, 0, msg.scores
		m.sortAll()
		return m, m.refreshCopies()
	case detailMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			m.mode, m.detail = modeList, nil
			return m, m.load()
		}
		m.detail = msg.st
		return m, nil
	case doneMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.status, false)
		}
		var cmds []tea.Cmd
		if msg.back {
			m.mode, m.detail = modeList, nil
		}
		if msg.reload {
			cmds = append(cmds, m.load())
			if m.mode == modeDetail && m.detail != nil {
				cmds = append(cmds, m.loadDetail(m.detail.Card.ID))
			}
		}
		return m, tea.Batch(cmds...)
	case planMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.plan, m.prompt = msg.plan, promptErase
		m.input.Reset()
		m.input.Prompt = "type the code " + msg.plan.Code + " to erase › "
		m.input.Placeholder = ""
		return m, m.input.Focus()
	case editedMsg:
		return m, m.applyEdit(msg)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.prompt != promptNone {
			return m, m.keyPrompt(msg)
		}
		if m.legend {
			switch msg.String() {
			case "?", "esc", "q":
				m.legend = false
			case "j", "down", "J":
				m.legendAt++
			case "k", "up", "K":
				m.legendAt = max(0, m.legendAt-1)
			}
			return m, nil
		}
		if msg.String() == "?" {
			m.legend, m.legendAt = true, 0
			return m, nil
		}
		if m.planView != nil {
			return m, m.keyPlans(msg)
		}
		if m.mode == modeDetail {
			return m, m.keyDetail(msg)
		}
		return m, tea.Batch(m.keyList(msg), m.refreshCopies())
	}
	return m, nil
}

func (m *model) listHeight() int { return max(3, m.h-4) }

func (m *model) move(d int) {
	n := len(m.visible())
	if n == 0 {
		m.sel = 0
		return
	}
	m.sel = max(0, min(n-1, m.sel+d))
}

func (m *model) keyList(k tea.KeyPressMsg) tea.Cmd {
	m.status = ""
	switch k.String() {
	case "q":
		return tea.Quit
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "g", "home":
		m.sel = 0
	case "G", "end":
		m.move(len(m.visible()))
	case "pgdown", "ctrl+d":
		m.move(m.listHeight())
	case "pgup", "ctrl+u":
		m.move(-m.listHeight())
	case "/":
		m.prompt = promptFilter
		m.input.Reset()
		m.input.SetValue(m.filter)
		m.input.Prompt = "search › "
		m.input.Placeholder = "words; enter searches titles, cards and text"
		return m.input.Focus()
	case "s":
		opts := append([]string{""}, m.spheres...)
		i := slices.Index(opts, m.sphere)
		m.sphere = opts[(i+1)%len(opts)]
		m.sel = 0
		return m.load()
	case "O":
		m.orphaned = !m.orphaned
		m.sel = 0
		return m.load()
	case "P":
		m.pending = !m.pending
		m.sel = 0
		return m.load()
	case "A":
		m.archived = !m.archived
		m.sel = 0
		return m.load()
	case "t":
		m.order = m.nextOrder()
		m.sortAll()
		m.sel = 0
		m.setStatus("sorted by "+m.orderLabel(), false)
	case "T":
		o := m.effectiveOrder()
		o.Desc = !o.Desc
		m.order = o
		m.sortAll()
		m.sel = 0
		m.setStatus("sorted by "+m.orderLabel(), false)
	case "r":
		m.filter, m.searched, m.classFilter = "", false, treeRow{}
		return m.load()
	case "C":
		if m.classFilter.plan != "" {
			m.classFilter, m.sel = treeRow{}, 0
			return m.load()
		}
		return m.openPlans(nil)
	case "c":
		if it := m.current(); it != nil {
			cp := *it
			return m.openPlans(&cp)
		}
	case "e", "E":
		if it := m.current(); it != nil {
			st, _, err := m.st.Stat([]string{it.URI}, false)
			if err != nil || len(st) == 0 {
				m.setStatus("cannot read the card", true)
				return nil
			}
			if k.String() == "e" {
				return m.edit(st[0].Card)
			}
			return m.editContent(st[0].Card)
		}
	case "enter", "l", "right":
		if it := m.current(); it != nil {
			m.mode, m.detail, m.scroll = modeDetail, nil, 0
			return m.loadDetail(it.URI)
		}
	case "o":
		if it := m.current(); it != nil {
			return m.open(it.URI, it.Mime)
		}
	}
	return nil
}

func (m *model) keyDetail(k tea.KeyPressMsg) tea.Cmd {
	m.status = ""
	if m.detail == nil {
		if s := k.String(); s == "esc" || s == "q" || s == "h" || s == "left" {
			m.mode = modeList
		}
		return nil
	}
	id := m.detail.URI
	switch k.String() {
	case "q", "esc", "h", "left", "backspace":
		m.mode, m.detail = modeList, nil
		return m.load()
	case "j", "down":
		m.scroll++
	case "k", "up":
		m.scroll = max(0, m.scroll-1)
	case "o":
		return m.open(id, m.detail.Card.Mime)
	case "e":
		return m.edit(m.detail.Card)
	case "E":
		return m.editContent(m.detail.Card)
	case "c":
		c := m.detail.Card
		return m.openPlans(&store.ListItem{Row: index.Row{ID: c.ID, Title: c.Title}, Sphere: c.Sphere, URI: m.detail.URI})
	case "a":
		toArchive := m.detail.Card.Archived == ""
		return func() tea.Msg {
			if _, err := m.st.Archive(id, toArchive, "tui"); err != nil {
				return doneMsg{err: err}
			}
			if toArchive {
				return doneMsg{status: "archived", reload: true}
			}
			return doneMsg{status: "active again", reload: true}
		}
	case "S":
		if m.detail.Card.Sealed {
			m.setStatus("already sealed", false)
			return nil
		}
		m.prompt = promptSeal
		m.input.Reset()
		m.input.Prompt = "seal and erase the clear versions? type yes › "
		m.input.Placeholder = ""
		return m.input.Focus()
	case "x", "X":
		m.force = k.String() == "X"
		force := m.force
		return func() tea.Msg {
			p, err := m.st.PlanErase(store.EraseRequest{Refs: []string{id}, Force: force})
			return planMsg{p, err}
		}
	}
	return nil
}

func (m *model) keyPrompt(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		if m.prompt == promptErase {
			m.setStatus("erasure cancelled; the code is left unused and expires", false)
		}
		if m.prompt == promptRevise && m.toSave != nil {
			m.toSave.seen = m.toSave.mtime()
		}
		m.prompt, m.plan, m.reveal, m.toSave = promptNone, nil, nil, nil
		m.input.Blur()
		return nil
	case "enter":
		p, val := m.prompt, strings.TrimSpace(m.input.Value())
		m.prompt = promptNone
		m.input.Blur()
		switch p {
		case promptFilter:
			m.filter, m.sel = val, 0
			if val != "" {
				sphere, state := m.sphere, "active"
				if m.archived {
					state = "archived"
				}
				return func() tea.Msg {
					hits, err := m.st.Search(val, sphere, 500, state, store.Sort{})
					if err != nil {
						return listMsg{err: err}
					}
					items := make([]store.ListItem, len(hits))
					scores := map[string]float64{}
					for i, h := range hits {
						items[i] = h.ListItem
						scores[h.URI] = h.Score
					}
					return searchMsg{items, scores}
				}
			}
			return m.load()
		case promptRevise:
			e := m.toSave
			m.toSave = nil
			if e == nil {
				return nil
			}
			if !strings.EqualFold(val, "yes") {
				e.seen = e.mtime() // asked once for this change
				m.setStatus("not saved; the copy stays at "+e.path, false)
				return nil
			}
			return func() tea.Msg {
				r, err := m.st.Revise(e.id, e.path)
				if err != nil {
					return doneMsg{err: err}
				}
				e.seen, e.sum = e.mtime(), sumFile(e.path)
				return doneMsg{status: fmt.Sprintf("saved as version v%d", r.Version), reload: true}
			}
		case promptReveal:
			req := m.reveal
			m.reveal = nil
			if req == nil || !strings.EqualFold(val, "yes") {
				m.setStatus("not opened", false)
				return nil
			}
			p, err := m.decrypt(req.id)
			if err != nil {
				m.setStatus(err.Error(), true)
				return nil
			}
			return m.openPath(p, req.mime, "decrypted copy")
		case promptSeal:
			if !strings.EqualFold(val, "yes") {
				m.setStatus("not sealed", false)
				return nil
			}
			id := m.detail.URI
			return func() tea.Msg {
				r, err := m.st.Seal(id)
				if err != nil {
					return doneMsg{err: err}
				}
				return doneMsg{status: fmt.Sprintf("sealed; %d clear version(s) erased", r.VersionsErased), reload: true}
			}
		case promptErase:
			plan := m.plan
			m.plan = nil
			if !strings.EqualFold(val, plan.Code) {
				m.setStatus("erasure cancelled: the code was not typed", false)
				return nil
			}
			return func() tea.Msg {
				proof, err := m.st.ConfirmErase(plan.Code)
				if err != nil {
					return doneMsg{err: err}
				}
				ok := 0
				for _, l := range proof.Locations {
					if l.Verified {
						ok++
					}
				}
				return doneMsg{status: fmt.Sprintf("erased; %d location(s) purged and verified", ok), reload: true, back: true}
			}
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if m.prompt == promptFilter {
		// Live filtering on the loaded list; enter searches the full text.
		m.filter, m.sel, m.searched = strings.TrimSpace(m.input.Value()), 0, false
	}
	return cmd
}

type revealReq struct{ id, mime string }

// open opens an artefact the smart way: a text payload in $EDITOR, inside
// the terminal; anything else in its default application. A sealed one is
// decrypted into a temporary file once the user confirms.
func (m *model) open(id string, mime string) tea.Cmd {
	if r, err := m.st.Resolve(id); err == nil && r.Row.Sealed {
		m.reveal = &revealReq{id, mime}
		m.prompt = promptReveal
		m.input.Reset()
		m.input.Prompt = "sealed: decrypt into a temporary file, deleted when the TUI quits? type yes › "
		m.input.Placeholder = ""
		return m.input.Focus()
	}
	p, from, err := m.locate(m.cfg, m.st, id)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	return m.openPath(p, mime, from)
}

func (m *model) openPath(p, mime, from string) tea.Cmd {
	if readsAsText(mime, p) {
		cmd := exec.Command("sh", "-c", editorCommand()+` "$1"`, "sh", p)
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			if err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{status: "read in the editor, from the " + from + " (changes are not saved to the artefact)"}
		})
	}
	return func() tea.Msg {
		if err := exec.Command("open", p).Run(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "opened from the " + from}
	}
}

// readsAsText reports whether a payload reads best in a text editor.
func readsAsText(mime, path string) bool {
	if strings.HasPrefix(mime, "text/") || strings.HasSuffix(mime, "+xml") || strings.HasSuffix(mime, "+json") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/toml", "application/javascript", "application/x-sh":
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt", ".yaml", ".yml", ".json", ".toml", ".csv", ".tsv", ".log", ".ini", ".conf", ".xml", ".html", ".tex", ".org", ".rst", ".sh", ".py", ".go":
		return true
	}
	return false
}

func editorCommand() string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if e := os.Getenv(v); e != "" {
			return e
		}
	}
	return "vi"
}

// editable is the part of a card the editor shows.
type editable struct {
	Title    string         `yaml:"title"`
	Date     string         `yaml:"date"`
	Filename string         `yaml:"filename"`
	Mime     string         `yaml:"mime"`
	Tags     []string       `yaml:"tags,flow"`
	DC       map[string]any `yaml:"dc"`
	X        map[string]any `yaml:"x"`
}

func (m *model) edit(c *store.Card) tea.Cmd {
	m.editCard = c
	b, _ := yaml.Marshal(editable{c.Title, c.Date, c.Filename, c.Mime, c.Tags, c.DC, c.X})
	f, err := os.CreateTemp("", "artefact-card-*.yaml")
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	header := "# Card of " + c.ID + ". Edit, save and quit; the other fields are kept by artefact.\n"
	f.WriteString(header)
	f.Write(b)
	f.Close()
	id, path := c.ID, f.Name()
	cmd := exec.Command("sh", "-c", editorCommand()+` "$1"`, "sh", path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return editedMsg{id: id, path: path, err: err} })
}

func (m *model) applyEdit(e editedMsg) tea.Cmd {
	defer os.Remove(e.path)
	if e.err != nil {
		m.setStatus("editor: "+e.err.Error(), true)
		return nil
	}
	b, err := os.ReadFile(e.path)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	var after editable
	if err := yaml.Unmarshal(b, &after); err != nil {
		m.setStatus("card not changed: "+err.Error(), true)
		return nil
	}
	cur := m.editCard
	if cur == nil {
		return nil
	}
	var sets []string
	if after.Title != cur.Title {
		sets = append(sets, "title="+after.Title)
	}
	if after.Date != cur.Date {
		sets = append(sets, "date="+after.Date)
	}
	if after.Filename != cur.Filename {
		sets = append(sets, "filename="+after.Filename)
	}
	if after.Mime != cur.Mime {
		sets = append(sets, "mime="+after.Mime)
	}
	if strings.Join(after.Tags, ",") != strings.Join(cur.Tags, ",") {
		sets = append(sets, "tags="+strings.Join(after.Tags, ","))
	}
	for k := range cur.DC {
		if _, ok := after.DC[k]; !ok {
			sets = append(sets, "dc."+k+"=")
		}
	}
	merge := map[string]any{}
	if len(after.DC) > 0 {
		merge["dc"] = after.DC
	}
	x := map[string]any{}
	for ns, v := range after.X {
		x[ns] = v
	}
	for ns := range cur.X {
		if _, ok := after.X[ns]; !ok {
			x[ns] = nil
		}
	}
	if len(x) > 0 {
		merge["x"] = x
	}
	doc := ""
	if len(merge) > 0 {
		mb, _ := yaml.Marshal(merge)
		doc = string(mb)
	}
	if len(sets) == 0 && doc == "" {
		m.setStatus("card unchanged", false)
		return nil
	}
	id := e.id
	return func() tea.Msg {
		if _, err := m.st.EditCard(id, sets, doc); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "card saved", reload: true}
	}
}

// ------------------------------------------------------------------ view

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *model) render() string {
	var body string
	if m.planView != nil {
		return m.renderPlans() + "\n" + m.footer()
	}
	if m.legend {
		return block(m.legendLines()[min(m.legendAt, max(0, len(m.legendLines())-1)):], m.w, m.h-1) + "\n" + pad(helpLine("?", "close", "j/k", "scroll"), m.w)
	}
	if m.mode == modeDetail {
		body = m.renderDetail()
	} else {
		body = m.renderList()
	}
	return body + "\n" + m.footer()
}

func (m *model) header() string {
	scope := "all spheres"
	if m.sphere != "" {
		scope = tag(m.sphere)
	}
	extra := ""
	if m.orphaned {
		extra += sWarn.Render("  orphaned only")
	}
	if m.pending {
		extra += sWarn.Render("  classification pending")
	}
	if m.archived {
		extra += sWarn.Render("  archives")
	}
	if cf := m.classFilter; cf.plan != "" {
		label := cf.label
		if cf.notation != "" && cf.notation != unclassifiedRow {
			label = cf.notation + " " + cf.label
		}
		extra += sMuted.Render("  class: ") + sText.Render(cf.plan+" › "+label)
	}
	if m.filter != "" {
		extra += sMuted.Render("  filter: ") + sText.Render(m.filter)
	}
	extra += sMuted.Render("  sort: ") + sText.Render(m.orderLabel())
	return sTitle.Render("artefact") + sMuted.Render("  ·  ") + scope + sMuted.Render(fmt.Sprintf("  ·  %d artefact(s)", len(m.visible()))) + extra
}

func (m *model) renderList() string {
	lines := []string{pad(m.header(), m.w), pad(m.syncLine(), m.w)}
	l := m.visible()
	h := m.listHeight() - 1
	if m.sel < m.top {
		m.top = m.sel
	}
	if m.sel >= m.top+h {
		m.top = m.sel - h + 1
	}
	if len(l) == 0 {
		lines = append(lines, sMuted.Render("  no artefact here. Deposit one with `artefact put <file>`."))
	}
	for i := m.top; i < len(l) && i < m.top+h; i++ {
		it := l[i]
		lock := " "
		if it.Sealed {
			lock = sWarn.Render("◆")
		}
		row := fmt.Sprintf(" %s%s %s %s %s %s %s %8s  %s",
			lock, copyMark(m.copies[it.ID]), sMuted.Render(it.ID), pad(tag(it.Sphere), 6), sMuted.Render(it.Date), pad(sMuted.Render("v"+fmt.Sprint(it.Version)), 3),
			pad(sMuted.Render(it.Mime), 24), size(it.Size), sText.Render(it.Title))
		row = pad(row, m.w)
		if i == m.sel {
			row = selectLine(row, m.w)
		}
		lines = append(lines, row)
	}
	return block(lines, m.w, m.h-2)
}

func (m *model) renderDetail() string {
	if m.detail == nil {
		return block([]string{m.header(), "", sMuted.Render("  loading…")}, m.w, m.h-2)
	}
	d, c := m.detail, m.detail.Card
	field := func(k, v string) string { return sMuted.Render(fmt.Sprintf("  %-10s ", k)) + sText.Render(v) }
	state := sOK.Render("clear")
	if c.Sealed {
		state = sWarn.Render("sealed")
	}
	if c.Archived != "" {
		state += "  " + sWarn.Render("archived "+c.Archived)
	}
	lines := []string{
		sTitle.Render(c.Title) + "  " + tag(c.Sphere) + "  " + state,
		sMuted.Render("  " + d.URI),
		"",
		field("date", c.Date),
		field("file", c.Filename),
		field("type", c.Mime),
		field("size", size(c.Size)),
		field("digest", c.Digest),
		field("deposited", c.Deposited),
		field("updated", c.Updated),
	}
	if len(c.Tags) > 0 {
		lines = append(lines, field("tags", strings.Join(c.Tags, ", ")))
	}
	if e := c.Embedded; e != nil {
		lines = append(lines, "", sBold.Render("In the file"))
		add := func(k, v string) {
			if v != "" && v != "0" {
				lines = append(lines, field(k, v))
			}
		}
		add("date", e.Date)
		add("title", e.Title)
		add("author", e.Author)
		add("subject", e.Subject)
		add("keywords", strings.Join(e.Keywords, ", "))
		add("camera", e.Camera)
		if len(e.GPS) == 2 {
			add("place", fmt.Sprintf("%.5f, %.5f", e.GPS[0], e.GPS[1]))
		}
		add("pages", fmt.Sprint(e.Pages))
		if e.Width > 0 {
			add("size", fmt.Sprintf("%d × %d", e.Width, e.Height))
		}
		if e.Duration > 0 {
			add("duration", fmt.Sprintf("%.0f s", e.Duration))
		}
		add("made by", e.Producer)
	}
	if len(c.DC) > 0 {
		lines = append(lines, "", sBold.Render("Dublin Core"))
		for _, k := range sortedKeys(c.DC) {
			lines = append(lines, field(k, fmt.Sprint(c.DC[k])))
		}
	}
	if len(c.X) > 0 {
		lines = append(lines, "", sBold.Render("Extensions"))
		for _, k := range sortedKeys(c.X) {
			lines = append(lines, field(k, fmt.Sprint(c.X[k])))
		}
	}
	if len(c.Classes) > 0 {
		lines = append(lines, "", sBold.Render("Classes"))
		plans := make([]string, 0, len(c.Classes))
		for p := range c.Classes {
			plans = append(plans, p)
		}
		slices.Sort(plans)
		for _, p := range plans {
			k := c.Classes[p]
			node := k.Node
			if node == "" {
				node = "—"
			}
			state := sMuted.Render(fmt.Sprintf("%s, %.2f", k.By, k.Confidence))
			if k.Pending {
				state = sWarn.Render("pending") + sMuted.Render(", "+k.By)
			}
			lines = append(lines, field(p, node)+"  "+state)
		}
	}
	if len(d.Copies) > 0 {
		lines = append(lines, "", sBold.Render("Copies"))
		lines = append(lines, field("local", sOK.Render("complete")))
		for _, cp := range d.Copies {
			st := sOK.Render("complete")
			if !cp.Complete {
				st = sWarn.Render(fmt.Sprintf("%d file(s) missing", cp.Missing))
			}
			if cp.Verified != "" {
				st += sMuted.Render("  verified " + cp.Verified)
			}
			lines = append(lines, field(cp.Backend, st))
		}
	}
	lines = append(lines, "", sBold.Render(fmt.Sprintf("Versions (%d)", len(d.Versions))))
	for _, v := range d.Versions {
		lines = append(lines, sMuted.Render(fmt.Sprintf("  %-4s %s  %s  %s", v.Version, v.Created, v.Digest, v.Message)))
	}
	lines = append(lines, "", sBold.Render(fmt.Sprintf("Holders (%d)", len(c.Holders))))
	if len(c.Holders) == 0 {
		lines = append(lines, sMuted.Render("  none: this artefact is orphaned"))
	}
	for _, hd := range c.Holders {
		lines = append(lines, sText.Render(fmt.Sprintf("  %s", hd.By))+sMuted.Render(fmt.Sprintf("  %s  %s", hd.Ref, hd.At)))
	}
	lines = append(lines, "", sBold.Render(fmt.Sprintf("Events (%d)", len(d.Events))))
	for i := len(d.Events) - 1; i >= 0; i-- {
		e := d.Events[i]
		v := ""
		if e.Version > 0 {
			v = fmt.Sprintf(" v%d", e.Version)
		}
		lines = append(lines, sMuted.Render(fmt.Sprintf("  %s  %-9s%s  %s  %s", e.At, e.Type, v, e.Agent, e.Outcome)))
	}
	if m.plan != nil {
		lines = append(lines, "", sErr.Render("Erasure plan"))
		for _, it := range m.plan.Items {
			what := "whole artefact, " + strings.Join(it.Versions, ", ")
			if !it.Whole {
				what = "versions " + strings.Join(it.Versions, ", ")
			}
			lines = append(lines, sText.Render("  "+it.URI+"  "+what))
			if len(it.Holders) > 0 {
				lines = append(lines, sWarn.Render(fmt.Sprintf("  held by %d: erasing anyway (force)", len(it.Holders))))
			}
		}
		lines = append(lines, sMuted.Render("  code valid until "+m.plan.Expires+"; nothing is erased until it is typed"))
	}
	h := max(1, m.h-2)
	m.scroll = min(m.scroll, max(0, len(lines)-h))
	return block(lines[m.scroll:], m.w, h)
}

// copyMark shows whether an artefact's copies are all on their backends.
func copyMark(c store.CopyState) string {
	switch c {
	case store.CopiesComplete:
		return sOK.Render("●")
	case store.CopiesPartial:
		return sWarn.Render("◐")
	case store.CopiesNone:
		return sErr.Render("○")
	case store.CopiesNoBackend:
		return sMuted.Render("·")
	}
	return " "
}

// syncLine sums up the replication: running syncs first.
func (m *model) syncLine() string {
	var parts []string
	for _, p := range m.progress {
		if m.sphere != "" && p.Sphere != m.sphere {
			continue
		}
		if p.Running {
			parts = append(parts, sWarn.Render("⟳ ")+sText.Render(fmt.Sprintf("%s → %s  %d/%d files, %s", p.Sphere, p.Backend, p.Done, p.Total, size(p.Bytes))))
		} else {
			parts = append(parts, sOK.Render("✓ ")+sMuted.Render(fmt.Sprintf("%s → %s  last sync %s", p.Sphere, p.Backend, shortTime(p.Updated))))
		}
	}
	if len(parts) == 0 {
		return sMuted.Render("no sync recorded yet")
	}
	return strings.Join(parts, sMuted.Render("   "))
}

func shortTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("02.01 15:04")
	}
	return s
}

func (m *model) footer() string {
	var help string
	switch {
	case m.prompt != promptNone:
		help = m.input.View()
	case m.mode == modeDetail:
		help = helpLine("o", "open", "E", "edit content", "e", "edit card", "a", "archive", "S", "seal", "x", "erase", "esc", "back", "?", "keys")
	default:
		help = helpLine("enter", "detail", "o", "open", "e/E", "edit card/content", "C", "plans", "c", "classify", "/", "search", "t/T", "sort", "?", "keys", "q", "quit")
	}
	status := ""
	if m.status != "" {
		st := sOK
		if m.statusErr {
			st = sErr
		}
		status = st.Render(m.status)
	}
	return pad(status, m.w) + "\n" + pad(help, m.w)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// legendLines lists the keys and marks, as office's TUI does with ?.
func (m *model) legendLines() []string {
	out := []string{sTitle.Render("Keys and legend") + sMuted.Render("  ·  ? closes it  ·  j k scroll"), ""}
	section := func(name string) { out = append(out, "", sSection.Render(name), "") }
	keyW := 14
	for _, theme := range []struct {
		title string
		keys  [][2]string
	}{
		{"List", [][2]string{
			{"j k  ↑ ↓", "move"}, {"g  G", "top, end"}, {"ctrl+d  ctrl+u", "page down, up"},
			{"enter  l", "detail of the artefact"},
			{"o", "open: a text file in $EDITOR, any other in its application; a sealed one after yes, decrypted into a temporary file deleted on quit"},
			{"/", "search: live filter while typing; enter searches titles, cards and text"},
			{"r", "refresh, and clear the search"},
			{"s", "sphere: all, then each sphere in turn"},
			{"O", "orphaned only: artefacts no tool holds"},
			{"P", "pending only: classification waits for you"},
			{"A", "archives: the archived artefacts instead of the active ones"},
			{"t", "sort: deposited, date, updated, title, size (and relevance after a search), in turn"},
			{"T", "reverse the sort: ↓ newest, largest, most relevant first, or Z-A; ↑ the other way"},
			{"e  E", "edit the card, or the content, of the selected artefact"},
			{"C", "plans: the classes with their counts; enter lists a class (sub-classes included); C again clears it"},
			{"c", "classify the selected artefact: pick its class in the plan (your decision, never overwritten)"},
		}},
		{"Detail", [][2]string{
			{"o", "open, as in the list"},
			{"e", "edit the card in $EDITOR: title, date, file name, type, dc, x"},
			{"c", "classify: pick its class in the plan"},
			{"E", "edit the content: text in $EDITOR, other files in their application; a changed copy is saved as the next version (the earlier ones are kept)"},
			{"a", "archive, or make active again"},
			{"S", "seal: encrypt, and erase the clear versions everywhere (type yes)"},
			{"x", "erase: shows the plan, asks for its code"},
			{"X", "erase an artefact a tool still holds (your override)"},
			{"j k", "scroll"}, {"esc  h", "back to the list"},
		}},
		{"Everywhere", [][2]string{{"?", "these keys"}, {"q  ctrl+c", "quit"}}},
	} {
		section(theme.title)
		for _, kv := range theme.keys {
			out = append(out, sText.Render(fmt.Sprintf("  %-*s", keyW, kv[0]))+sMuted.Render(kv[1]))
		}
	}
	section("Marks of a row")
	for _, mk := range [][2]string{
		{sWarn.Render("◆"), "sealed: encrypted with the sphere's key"},
		{sOK.Render("●"), "copies complete on every backend of the sphere"},
		{sWarn.Render("◐"), "copies partial: the sync has started, or a file changed since"},
		{sErr.Render("○"), "no copy yet on the backends"},
		{sMuted.Render("·"), "the sphere has no backend"},
	} {
		out = append(out, "  "+mk[0]+"  "+sMuted.Render(mk[1]))
	}
	section("Sync line")
	out = append(out, "  "+sWarn.Render("⟳")+"  "+sMuted.Render("a sync is running: files sent out of files to send"),
		"  "+sOK.Render("✓")+"  "+sMuted.Render("the last sync finished at the time shown"))
	return out
}

// decrypt writes a sealed artefact's clear payload into a private temporary
// directory and remembers it for deletion.
func (m *model) decrypt(id string) (string, error) {
	rc, c, err := m.st.OpenPayload(id, "", true)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Caches", "artefact", "reveal", c.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := c.Filename
	if name == "" {
		name = c.ID + c.Ext
	}
	p := filepath.Join(dir, filepath.Base(name))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	m.revealed = append(m.revealed, dir)
	return p, nil
}

// Cleanup deletes the decrypted copies made during the session.
func (m *model) Cleanup() {
	for _, d := range m.revealed {
		os.RemoveAll(d)
	}
	m.revealed = nil
}

// edit is a working copy of an artefact's content.
type edit struct {
	id, name, path string
	sum            string    // SHA-256 of the copy as handed out, or last saved
	seen           time.Time // modification time already asked about
}

func (e *edit) mtime() time.Time {
	fi, err := os.Stat(e.path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

type editedContentMsg struct {
	id  string
	err error
}

func sumFile(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

// editContent hands out a working copy: text in $EDITOR, then a save is
// proposed on return; other files in their application, watched for saves.
func (m *model) editContent(c *store.Card) tea.Cmd {
	rc, _, err := m.st.OpenPayload(c.ID, "", c.Sealed)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	defer rc.Close()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Caches", "artefact", "edit", c.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	name := c.Filename
	if name == "" {
		name = c.ID + c.Ext
	}
	p := filepath.Join(dir, filepath.Base(name))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	_, err = io.Copy(f, rc)
	f.Close()
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	e := &edit{id: c.ID, name: name, path: p, sum: sumFile(p)}
	e.seen = e.mtime()
	m.edits[c.ID] = e
	if c.Sealed {
		m.revealed = append(m.revealed, dir)
	}
	if readsAsText(c.Mime, p) {
		cmd := exec.Command("sh", "-c", editorCommand()+` "$1"`, "sh", p)
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return editedContentMsg{id: c.ID, err: err} })
	}
	return func() tea.Msg {
		if err := exec.Command("open", p).Run(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "editing " + name + " in its application; save there, and the TUI offers a new version"}
	}
}

// askRevise asks whether a changed copy becomes the next version.
func (m *model) askRevise(e *edit) tea.Cmd {
	if sumFile(e.path) == e.sum {
		e.seen = e.mtime()
		m.setStatus("no change to "+e.name, false)
		return nil
	}
	m.toSave = e
	m.prompt = promptRevise
	m.input.Reset()
	m.input.Prompt = e.name + " changed: save as the next version? type yes › "
	m.input.Placeholder = ""
	return m.input.Focus()
}

// checkEdits looks for working copies saved since last asked.
func (m *model) checkEdits() tea.Cmd {
	if m.prompt != promptNone {
		return nil
	}
	for _, e := range m.edits {
		if mt := e.mtime(); mt.After(e.seen) {
			e.seen = mt
			return m.askRevise(e)
		}
	}
	return nil
}

func indexFilterAll() index.Filter { return index.Filter{} }

// who names the human behind a TUI decision.
func who() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "tui"
}

// effectiveOrder is the order in force, defaults resolved.
func (m *model) effectiveOrder() store.Sort {
	o := m.order
	if o.Key == "" || (o.Key == "relevance" && !m.searched) {
		key := "deposited"
		if m.searched {
			key = "relevance"
		}
		o = store.Sort{Key: key, Desc: store.DefaultDesc(key)}
	}
	return o
}

// nextOrder moves to the next sort key, in its natural direction.
func (m *model) nextOrder() store.Sort {
	keys := store.SortKeys[1:]
	if m.searched {
		keys = store.SortKeys
	}
	cur := m.effectiveOrder().Key
	i := slices.Index(keys, cur)
	key := keys[(i+1)%len(keys)]
	return store.Sort{Key: key, Desc: store.DefaultDesc(key)}
}

func (m *model) orderLabel() string {
	o := m.effectiveOrder()
	arrow := "↑"
	if o.Desc {
		arrow = "↓"
	}
	return o.Key + " " + arrow
}

func (m *model) sortAll() {
	var score func(store.ListItem) float64
	if m.searched {
		score = func(it store.ListItem) float64 { return m.scores[it.URI] }
	}
	store.SortList(m.all, m.effectiveOrder(), score)
}
