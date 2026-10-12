// Package tui is artefact's terminal interface (SPEC §13): the artefacts of
// the granted spheres, their card, versions, holders and events, and the
// actions open, edit card, seal and erase (plan, then code).
package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/notes"
	"github.com/aclemen1/tuikit"
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
	"github.com/aclemen1/artefact-cli/internal/query"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// Locator gives a local path to open an artefact (the server's Locate).
type Locator func(cfg *config.Config, st *store.Store, id string) (string, string, error)

// Run shows the TUI over a store already scoped to the spheres it may use.
func Run(cfg *config.Config, st *store.Store, locate Locator, version, selectID string) error {
	tuikit.SetLanguage(tuikit.English) // as the rest of the TUI; dates are still read in French too
	m := newModel(cfg, st, locate)
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		m.exe = exe
		m.exeStamp, _ = stampOf(exe)
	}
	m.build = buildLabel(version, m.exe)
	m.loadPrefs(prefsPath())
	m.loadResume()
	if m.resumed == nil {
		m.selectOnStart(selectID)
	}
	p := tea.NewProgram(m)
	stop := watchSignals(p)
	_, err := p.Run()
	stop()
	m.Cleanup()
	if err == nil && m.reexec {
		return execSelf(m.exe)
	}
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

	copies       map[string]store.CopyState
	progress     []store.SyncProgress
	legend       bool
	legendAt     int
	reveal       *revealReq        // sealed artefact waiting for the confirmation to open it
	edits        map[string]*edit  // content being edited in an application, by artefact
	toSave       *edit             // edited copy waiting for the confirmation to save it
	editCard     *store.Card       // card open in the editor
	planView     *planView         // the plans tree, when shown
	treeOpen     map[string]bool   // classes open in the plans tree, kept between sessions
	prefsFile    string            // where the preferences are kept; "" keeps none (tests)
	saved        prefs             // the preferences as last written
	startInPlans bool              // the last session ended in the plans tree
	searching    string            // the phase of the search that runs: words, meaning; "" when none
	searchSeq    int               // the search whose results are awaited
	versions     map[string]int64  // index versions last seen, by sphere
	signatures   map[string]string // artefact count and last card change, by sphere
	stale        bool              // another process changed the store; reload when free
	exe          string            // the running binary, watched for a rebuild
	exeStamp     binStamp          // its time and inode at start
	newBinary    bool              // a new binary waits for the TUI to be at rest
	reexec       bool              // quit to exec the new binary
	pendingG     bool              // g pressed: a second g goes to the top
	target       string            // artefact a prompt acts on (seal)
	modal        *tuikit.Modal     // the open input, if any: it takes every key
	selecting    *selectReq        // --select, applied once the list is read
	noteFor      *store.ListItem   // artefact the open note editor writes to
	build        string            // version and commit, shown in the header
	resumed      *resume           // state carried over a reload, applied once the list is read
	classFilter  treeRow           // list filtered on a class (plan "" for none)
	revealed     []string          // decrypted temporary files, deleted on quit
	busy         *tuikit.Busy      // the background jobs, shown in the header; ! lists them
	ended        *ended            // jobs ended, whose reload or back is still to apply
	focus        *tuikit.Focus     // the selection, written to the shared focus file
	focusRef     string            // the ref last handed to focus
	greffierFor  [2]string         // ref and sphere the open greffier line is typed on
	undoing      *undoPreviewMsg   // the undo waiting for its confirmation
	journal      []historyEntry    // the journal of the artefact in detail

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
	return &model{cfg: cfg, st: st, locate: locate, spheres: st.Spheres, input: in, w: 100, h: 30, copies: map[string]store.CopyState{}, edits: map[string]*edit{}, treeOpen: map[string]bool{}, busy: tuikit.NewBusy(), ended: &ended{}, focus: tuikit.NewFocus("artefact")}
}

// ------------------------------------------------------------------ data

type (
	listMsg struct {
		items []store.ListItem
		err   error
	}
	detailMsg struct {
		st      *store.Stat
		journal []historyEntry
		err     error
	}
	doneMsg struct {
		status string
		err    error
		reload bool
		back   bool
		say    bool // the status stays in the status line once the job ended (greffier, undo)
		warn   bool // the status says something to look at
	}
	planMsg struct {
		plan *store.Plan
		err  error
	}
	searchMsg struct {
		items  []store.ListItem
		scores map[string]float64
		note   string
		seq    int
		phase  string  // words, then meaning
		next   tea.Cmd // the search by meaning, after the words
		err    error
	}
	tickMsg   struct{}
	copiesMsg struct {
		states   map[string]store.CopyState
		progress []store.SyncProgress
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
	cmd := func() tea.Msg {
		st, missing, err := m.st.Stat([]string{id}, true)
		if err == nil && len(missing) > 0 {
			err = fmt.Errorf("artefact %s is gone", id)
		}
		if err != nil {
			return detailMsg{err: err}
		}
		if notes.Configured(m.cfg) {
			// A note tool that fails leaves the detail shown without notes.
			st[0].Notes, _ = notes.Of(m.cfg, st[0].Card.ID, st[0].Card.Sphere)
		}
		// A journal that fails stays discreet: the section is hidden.
		journal, _ := historyOf(m.cfg, st[0].Card.ID, st[0].Card.Sphere)
		return detailMsg{st: &st[0], journal: journal}
	}
	if notes.Configured(m.cfg) || len(m.cfg.History.Ls) > 0 {
		return m.wrap("read the notes and journal of "+shortID(id), cmd) // outside tools
	}
	return cmd
}

// visible is the list once the text filter applies.
func (m *model) visible() []store.ListItem {
	if m.filter == "" || m.searched {
		return m.all
	}
	// While typing, the plain words filter; operators wait for enter.
	var words []string
	for _, w := range strings.Fields(m.filter) {
		if strings.ContainsAny(w, `:"()`) || strings.HasPrefix(w, "-") || w == "OR" {
			continue
		}
		words = append(words, query.Fold(w))
	}
	if len(words) == 0 {
		return m.all
	}
	var out []store.ListItem
	for _, it := range m.all {
		hay := query.Fold(it.Title + " " + it.Filename + " " + it.ID + " " + it.Mime)
		keep := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, it)
		}
	}
	return out
}

func (m *model) current() *store.ListItem {
	if m.planView != nil {
		return m.treeItem()
	}
	l := m.visible()
	if m.sel < 0 || m.sel >= len(l) {
		return nil
	}
	return &l[m.sel]
}

func (m *model) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

// ------------------------------------------------------------------ update

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.RequestBackgroundColor, m.load(), tick()}
	if m.startInPlans {
		cmds = append(cmds, m.openPlans(nil))
	}
	return tea.Batch(cmds...)
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

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := m.busy.Update(msg); ok {
		return m, tea.Batch(cmd, m.afterJobs())
	}
	if f, ok := msg.(failedMsg); ok {
		// Busy shows the failure; the view takes the message, without a status.
		status, isErr := m.status, m.statusErr
		next, cmd := m.update(f.inner)
		m.status, m.statusErr = status, isErr
		return next, cmd
	}
	if cmd, ok := m.onModal(msg); ok {
		return m, cmd
	}
	if m.modal.Open() {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
			return m, m.modal.Update(msg) // no key of the TUI while typing
		case tea.WindowSizeMsg:
		default:
			// The rest (ticks, reloads) runs as usual; the modal sees it too.
			cmd := m.modal.Update(msg)
			_, rest := m.updateBase(msg)
			return m, tea.Batch(cmd, rest)
		}
	}
	return m.updateBase(msg)
}

func (m *model) updateBase(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that reports no size still gets a usable layout.
		m.w, m.h = msg.Width, msg.Height
		if m.w < 20 || m.h < 6 {
			m.w, m.h = max(m.w, 80), max(m.h, 24)
		}
		m.input.SetWidth(max(10, m.w-30))
		if m.modal.Open() {
			m.modal.SetSize(m.w, m.h)
		}
		return m, nil
	case tea.BackgroundColorMsg:
		darkBackground = msg.IsDark()
		return m, nil
	case listMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		selected := ""
		if it := m.current(); it != nil && m.planView == nil {
			selected = it.URI
		}
		m.all, m.searched = msg.items, false
		m.sortAll()
		m.sel = min(m.sel, max(0, len(m.visible())-1))
		if selected != "" {
			for i, it := range m.visible() {
				if it.URI == selected {
					m.sel = i
					break
				}
			}
		}
		if m.resumed != nil {
			return m, tea.Batch(m.refreshCopies(), m.resumeCmds())
		}
		if m.selecting != nil {
			return m, tea.Batch(m.refreshCopies(), m.applySelect())
		}
		return m, m.refreshCopies()
	case plansMsg:
		if m.planView != nil {
			if msg.err != nil {
				m.setStatus(msg.err.Error(), true)
			}
			m.planView.data = msg.data
			for _, d := range msg.data {
				if _, seen := m.planView.open[d.plan.ID+":"]; !seen {
					m.planView.open[d.plan.ID+":"] = true
				}
			}
			m.rebuildTree()
			if f := m.planView.focus; f != "" {
				m.planView.focus = ""
				m.focusTree(f)
			}
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.refreshCopies(), m.checkEdits(), m.watchStore(), m.checkBinary(), tick())
	case reloadMsg:
		m.newBinary = true
		return m, m.reloadIfIdle()
	case versionsMsg:
		return m, m.onVersions(msg)
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
		if msg.seq != m.searchSeq {
			return m, nil // a newer search runs
		}
		selected := ""
		if it := m.current(); it != nil && msg.phase == "meaning" {
			selected = it.URI
		}
		m.all, m.searched, m.sel, m.scores = msg.items, true, 0, msg.scores
		m.sortAll()
		if selected != "" {
			for i, it := range m.visible() {
				if it.URI == selected {
					m.sel = i
				}
			}
		}
		if msg.err != nil {
			m.searching = ""
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.setStatus(msg.note, false)
		if msg.phase == "words" && msg.next != nil {
			m.searching = "meaning"
			return m, tea.Batch(m.refreshCopies(), m.wrap("search by meaning: "+m.filter, msg.next))
		}
		m.searching = ""
		return m, m.refreshCopies()
	case detailMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			m.mode, m.detail = modeList, nil
			return m, m.load()
		}
		m.detail, m.journal = msg.st, msg.journal
		return m, nil
	case doneMsg:
		return m, m.onDone(msg, true)
	case undoPreviewMsg:
		return m, m.confirmUndo(msg)
	case planMsg:
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.plan = msg.plan
		return m, m.openModal("Erase", tuikit.NewConfirmTyped("erase", eraseQuestion(msg.plan), msg.plan.Code))
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.prompt != promptNone {
			return m, m.keyPrompt(msg) // q is typed there
		}
		// Every TUI of the ecosystem: q quits from any view, esc goes back.
		if msg.String() == "q" {
			return m, tea.Quit
		}
		if m.legend {
			switch msg.String() {
			case "?", "esc":
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
		if msg.String() == tuikit.BusyKey {
			return m, m.openJobs()
		}
		if cmd, ok := m.sharedKey(msg.String()); ok {
			m.pendingG = false
			return m, cmd
		}
		if m.mode == modeDetail {
			return m, m.keyDetail(msg)
		}
		if m.planView != nil {
			return m, m.keyPlans(msg)
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
	key := k.String()
	if key == "g" {
		if m.pendingG {
			m.pendingG, m.sel = false, 0 // gg: top
		} else {
			m.pendingG = true
		}
		return nil
	}
	m.pendingG = false
	if it := m.current(); it != nil {
		if cmd, ok := m.itemKey(key, *it, m.planView != nil); ok {
			return cmd
		}
	}
	switch key {
	case "esc", "h", "left":
		// Back from a search or a class to the whole list.
		if m.filter != "" || m.searched || m.classFilter.plan != "" {
			m.filter, m.searched, m.classFilter, m.sel = "", false, treeRow{}, 0
			return m.load()
		}
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "home":
		m.sel = 0
	case "G", "end":
		m.move(len(m.visible()))
	case "1", "2", "3", "4", "5":
		return m.goView(key)
	case "R":
		return m.syncNow()
	case "c":
		return m.askDeposit()
	case "pgdown", "ctrl+d":
		m.move(m.listHeight())
	case "pgup", "ctrl+u":
		m.move(-m.listHeight())
	case "/":
		m.prompt = promptFilter
		m.input.Reset()
		m.input.SetValue(m.filter)
		m.input.Prompt = "search › "
		m.input.Placeholder = `words filter as you type; enter searches text and meaning: "phrase" -word a OR b type:pdf class:banque after:2024 is:pending`
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
		return m.wrap("refresh", m.load())
	case "C":
		if m.classFilter.plan != "" {
			m.classFilter, m.sel = treeRow{}, 0
			return m.load()
		}
		focus := ""
		if it := m.current(); it != nil {
			focus = it.URI
		}
		cmd := m.openPlans(nil)
		m.planView.focus = focus
		return cmd
	case "enter", "l", "right", "tab":
		if it := m.current(); it != nil {
			m.mode, m.detail, m.scroll = modeDetail, nil, 0
			return m.loadDetail(it.URI)
		}
	}
	return nil
}

func (m *model) keyDetail(k tea.KeyPressMsg) tea.Cmd {
	m.status = ""
	if m.detail == nil {
		if s := k.String(); s == "esc" || s == "h" || s == "left" {
			m.mode = modeList
		}
		return nil
	}
	id := m.detail.URI
	switch k.String() {
	case "esc", "h", "left", "backspace", "tab":
		m.mode, m.detail = modeList, nil
		if m.planView != nil {
			return tea.Batch(m.load(), m.loadPlans())
		}
		return m.load()
	case "j", "down", "J":
		m.scroll++
	case "k", "up", "K":
		m.scroll = max(0, m.scroll-1)
	case "R":
		return m.syncNow()
	default:
		if r, err := m.st.Resolve(id); err == nil {
			it := store.ListItem{Row: r.Row, Sphere: r.Sphere, URI: id}
			if cmd, ok := m.itemKey(k.String(), it, false); ok {
				return cmd
			}
		}
	}
	return nil
}

func (m *model) keyPrompt(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.prompt = promptNone // the filter: the other inputs are modals
		m.input.Blur()
		return nil
	case "enter":
		p, val := m.prompt, strings.TrimSpace(m.input.Value())
		m.prompt = promptNone
		m.input.Blur()
		return m.finish(p, val)
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
		return m.confirm(promptReveal, "reveal", "Sealed artefact", "Decrypt it into a temporary file, deleted when the TUI quits?")
	}
	p, from, err := m.locate(m.cfg, m.st, id)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	return m.openPath(p, mime, from)
}

func (m *model) openPath(p, mime, from string) tea.Cmd {
	_ = os.Chmod(p, 0o400) // what o opens is read, by its owner only; e edits
	if readsAsText(mime, p) {
		cmd := exec.Command("sh", "-c", readOnlyEditor(editorCommand())+` "$1"`, "sh", p)
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			if err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{status: "read in the editor, read-only, from the " + from + " (e edits the content)"}
		})
	}
	return m.job("open "+filepath.Base(p), func() doneMsg {
		if err := exec.Command("open", p).Run(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "opened from the " + from}
	})
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

// readOnlyEditor adds the read-only option of the editors that have one.
func readOnlyEditor(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return cmd
	}
	switch filepath.Base(fields[0]) {
	case "vim", "nvim", "vi", "gvim", "mvim", "view":
		return cmd + " -M"
	case "nano":
		return cmd + " -v"
	case "micro":
		return cmd + " -readonly true"
	case "emacs", "emacsclient":
		return cmd + ` --eval "(setq-default buffer-read-only t)"`
	}
	return cmd
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

// saveCard saves the fields of after that differ from the card cur.
func (m *model) saveCard(cur *store.Card, after editable) tea.Cmd {
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
	id := cur.ID
	return m.job("save the card of "+cur.Title, func() doneMsg {
		if _, err := m.st.EditCard(id, sets, doc); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "card saved", reload: true}
	})
}

// ------------------------------------------------------------------ view

func (m *model) View() tea.View {
	v := tea.NewView(tuikit.Overlay(m.render(), m.modal, m.w, m.h))
	v.AltScreen = true
	return v
}

func (m *model) render() string {
	var body string
	if m.planView != nil && m.mode != modeDetail {
		return m.renderPlans() + "\n" + m.footer()
	}
	if m.legend {
		return block(m.legendLines()[min(m.legendAt, max(0, len(m.legendLines())-1)):], m.w, m.h-1) + "\n" + pad(helpLine("?/esc", "close", "j/k", "scroll", "q", "quit"), m.w)
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
	if m.newBinary {
		extra += "  " + sWarn.Render("new version: reloads when you are done")
	}
	if m.build != "" {
		extra += sMuted.Render("  ·  " + m.build)
	}
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
		row := fmt.Sprintf("  %s %s   %s   %s  %s  %s  %s %8s   %s",
			lock, copyMark(m.copies[it.ID]), sMuted.Render(shortID(it.ID)), pad(tag(it.Sphere), 6), sMuted.Render(it.Date), pad(sMuted.Render("v"+fmt.Sprint(it.Version)), 3),
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
	if notes.Configured(m.cfg) {
		lines = append(lines, "", sBold.Render(fmt.Sprintf("Notes (%d)", len(d.Notes)))+sMuted.Render("  ·  N adds one"))
		for _, n := range d.Notes {
			head := n.ID + "  " + n.Created
			if n.By != "" {
				head += "  " + n.By
			}
			lines = append(lines, sMuted.Render("  "+head))
			for _, l := range strings.Split(strings.TrimRight(n.Body, "\n"), "\n") {
				lines = append(lines, sText.Render("    "+l))
			}
		}
	}
	lines = append(lines, m.journalLines()...)
	lines = append(lines, "", sBold.Render(fmt.Sprintf("Events (%d)", len(d.Events))))
	for i := len(d.Events) - 1; i >= 0; i-- {
		e := d.Events[i]
		v := ""
		if e.Version > 0 {
			v = fmt.Sprintf(" v%d", e.Version)
		}
		who := e.By
		if who == "" {
			who = e.Agent
		}
		lines = append(lines, sMuted.Render(fmt.Sprintf("  %s  %-9s%s  %s  %s", e.At, e.Type, v, who, e.Outcome)))
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
	case m.planView != nil && m.mode != modeDetail && m.planView.assign == nil:
		help = helpLine("l/h", "open/close", "enter", "list class/detail", "o", "open", "f", "finder", "E/M", "card/content", "m", "classify", "x", "archive", "/", "search", "t/T", "sort", "i", "greffier", "u", "undo", "esc", "back", "?", "keys", "q", "quit")
	case m.mode == modeDetail:
		help = helpLine("o", "open", "f", "finder", "E", "edit card", "M", "edit content", "m", "classify", "x", "archive", "S", "seal", "#", "erase", "i", "greffier", "u", "undo", "@", "cabinet", "esc", "back", "?", "keys", "q", "quit")
	default:
		help = helpLine("enter", "detail", "o", "open", "f", "finder", "E/M", "card/content", "m", "classify", "x", "archive", "c", "new", "2", "plans", "/", "search", "t/T", "sort", "i", "greffier", "u", "undo", "esc", "back", "?", "keys", "q", "quit")
	}
	if m.prompt == promptNone && m.busy.Unread() > 0 {
		help = helpLine(tuikit.BusyKey, "jobs") + sMuted.Render(" · ") + help
	}
	status := ""
	if m.status != "" {
		st := sOK
		if m.statusErr {
			st = sErr
		}
		status = st.Render(m.status)
	}
	if b := m.busy.View(); b != "" {
		status = strings.TrimSpace(b + "   " + status)
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
		{"Move and views", [][2]string{
			{"j k  ↑ ↓", "move"}, {"gg  G", "top, end"}, {"ctrl+d  ctrl+u", "page down, up"},
			{"enter  l  tab", "detail of the artefact"}, {"esc  h", "back: from the detail, the plans, a search or a class (esc never quits)"},
			{"1", "the list"}, {"2  C", "the plans: classes and their artefacts, as a tree"},
			{"3  P", "pending: classification waits for you"}, {"4  A", "archives"}, {"5  O", "orphaned: artefacts no tool holds"},
			{"s", "sphere: all, then each sphere in turn"},
			{"t  T", "sort: deposited, date, updated, title, size (and relevance after a search), in turn; T reverses"},
			{"r", "refresh, and clear the search"}, {"R", "sync now: copy to the backends what is not there yet"},
			{"/", "search: the words filter while typing (accents ignored); enter searches titles, cards and texts, by words and by meaning"},
			{"", `  "phrase"  -word  a OR b  x AROUND(5) y  title: file: text: card:`},
			{"", "  type:pdf|image|text|docx  class:4.2|banque  plan:  tag:  -tag:  sphere:"},
			{"", "  after:2024-03  before:2025  year:2023  is:sealed|archived|active|pending|orphaned|classified|unclassified  -is:"},
		}},
		{"On the selected artefact (list, plans, detail)", [][2]string{
			{"o", "open, read-only: a text file in $EDITOR, any other in its application; a sealed one after yes, decrypted into a temporary file deleted on quit"},
			{"f", "show the file in the Finder (or the Linux file manager), selected: in the mounted view (from the plans, at its place in the plan), else the local copy"},
			{"E", "edit the card in $EDITOR: title, date, file name, type, tags, dc, x"},
			{"M", "edit the content: text in $EDITOR, other files in their application; a changed copy is saved as the next version (the earlier ones are kept)"},
			{"m", "place it in a class of the plan (your decision, never overwritten)"},
			{"e  x", "archive, or make active again (reversible)"},
			{"S", "seal: encrypt, and erase the clear versions everywhere (type yes)"},
			{"#", "erase for good: shows the plan, asks for its code"},
			{"X", "erase an artefact a tool still holds (your override)"},
			{"c", "deposit a file (new artefact): its path, then its sphere when none is in view"},
			{"N", "add a note to the artefact (shown in its detail)"},
			{"u", "undo the last write on the artefact (undo.preview, then a confirmation, then undo.last); a reverse that cannot be undone in turn is never run here: undo tui --select <id>"},
		}},
		{"Plans (2, C)", [][2]string{
			{"l  →", "open a class: its sub-classes, then its artefacts"},
			{"h  ←", "close the class, or go up to its parent"},
			{"enter", "on a class, list it (sub-classes included); on an artefact, its detail (esc comes back here)"},
			{"t  T", "sort the artefacts of each class; classes keep the order of their notation"},
		}},
		{"Detail", [][2]string{{"j k  J K", "scroll"}}},
		{"Everywhere", [][2]string{{"?", "these keys"}, {"i", "a free line for the greffier (greffier.text), on the selected artefact; its proposal and doubts come back in the status line"}, {"@", "open or show the CABINET pane (cabinet.open)"}, {tuikit.BusyKey, "background jobs: sync, deposits, searches, classes, opening…; a failure stays red in the status line until this list is opened"}, {"esc", "back to the previous view; never quits"}, {"q  ctrl+c", "quit, from any view (in a field, q is typed)"}}},
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
	section("Journal of the detail")
	out = append(out, "  "+sWarn.Render("↶ undoable")+"  "+sMuted.Render("the entry carries its reverse: u undoes the last one (shown when undo.last is configured)"))
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
	os.Remove(p) // an earlier copy is read-only (o opens read-only)
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
	os.Remove(p) // a copy left read-only would refuse the new content
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
	return m.job("open "+name+" to edit", func() doneMsg {
		if err := exec.Command("open", p).Run(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "editing " + name + " in its application; save there, and the TUI offers a new version"}
	})
}

// askRevise asks whether a changed copy becomes the next version.
func (m *model) askRevise(e *edit) tea.Cmd {
	if sumFile(e.path) == e.sum {
		e.seen = e.mtime()
		m.setStatus("no change to "+e.name, false)
		return nil
	}
	m.toSave = e
	return m.confirm(promptRevise, "revise", "New version", e.name+" changed: save it as the next version? The earlier versions are kept.")
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

// who is the actor of a TUI decision: always human:AC@tui.
func (m *model) who() string {
	if m.st.By != "" {
		return m.st.By
	}
	return actor.TUI
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
	keys := []string{"deposited", "date", "updated", "title", "size"}
	if m.searched {
		keys = append([]string{"relevance"}, keys...)
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

// shortID is the end of a ULID: its start is a timestamp that artefacts
// deposited together share. The detail view shows the whole id, and the
// filter (/) finds an artefact by these characters.
func shortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

type versionsMsg struct {
	versions   map[string]int64
	signatures map[string]string
}

// watchStore notices what other processes wrote: the index versions cost
// nearly nothing; only when one moved are the signatures read, and only a
// changed signature (artefacts or cards, not copies) means news.
func (m *model) watchStore() tea.Cmd {
	last := m.signatures
	return func() tea.Msg {
		v := m.st.DataVersions()
		return versionsMsg{versions: v, signatures: last}
	}
}

// onVersions reloads what is shown when another process changed the store:
// the list, the plans tree, the detail. It waits while a question is asked,
// and does not run a search again: it says that there is news.
func (m *model) onVersions(v versionsMsg) tea.Cmd {
	moved := m.versions == nil
	for sp, n := range v.versions {
		if old, seen := m.versions[sp]; seen && old != n {
			moved = true
		}
	}
	m.versions = v.versions
	if moved {
		sig := m.st.Signatures()
		for sp, x := range sig {
			if old, seen := m.signatures[sp]; seen && old != x {
				m.stale = true
			}
		}
		m.signatures = sig
	}
	if !m.stale || m.prompt != promptNone || m.editCard != nil {
		return nil
	}
	if m.searched {
		if m.status == "" {
			m.setStatus("the store changed: r refreshes", false)
		}
		return nil
	}
	m.stale = false
	cmds := []tea.Cmd{m.wrap("reload: the store changed", m.load())}
	if m.planView != nil {
		cmds = append(cmds, m.loadPlans())
	}
	if m.mode == modeDetail && m.detail != nil {
		cmds = append(cmds, m.loadDetail(m.detail.URI))
	}
	return tea.Batch(cmds...)
}

// startSearch searches by words first, which answers at once, then by
// meaning when a sphere has its index; Busy shows the phase.
func (m *model) startSearch(q, sphere, state string) tea.Cmd {
	m.searchSeq++
	seq := m.searchSeq
	m.searching = "words"
	m.status = ""
	off := false
	var run func(phase string, semantic *bool) tea.Msg
	run = func(phase string, semantic *bool) tea.Msg {
		res, err := m.st.Find(q, store.FindOptions{Sphere: sphere, Limit: 500, State: state, Semantic: semantic})
		if err != nil {
			return searchMsg{seq: seq, phase: phase, err: err}
		}
		items := make([]store.ListItem, len(res.Hits))
		scores := map[string]float64{}
		for i, h := range res.Hits {
			items[i] = h.ListItem
			scores[h.URI] = h.Score
		}
		note := fmt.Sprintf("%d found by words", len(items))
		if res.Semantic {
			note = fmt.Sprintf("%d found by words and meaning", len(items))
		}
		if res.Corrected != "" {
			note += " · searched for: " + res.Corrected
		}
		for _, n := range res.Notes {
			note += " · " + n
		}
		msg := searchMsg{items: items, scores: scores, note: note, seq: seq, phase: phase}
		if phase == "words" && m.meaningApplies(q, sphere) {
			msg.next = func() tea.Msg { return run("meaning", nil) }
		}
		return msg
	}
	return m.wrap("search by words: "+q, func() tea.Msg { return run("words", &off) })
}

// meaningApplies tells whether a search by meaning would add anything: the
// query has words, and a sphere in view has its index by meaning.
func (m *model) meaningApplies(q, sphere string) bool {
	parsed, err := query.Parse(q)
	if err != nil || parsed.Free() == "" {
		return false
	}
	for _, sp := range m.spheres {
		if (sphere == "" || sp == sphere) && m.st.SemanticReady(sp) {
			return true
		}
	}
	return false
}

// selectInList puts the list's selection on the artefact uri, when shown.
func (m *model) selectInList(uri string) {
	for i, it := range m.visible() {
		if it.URI == uri {
			m.sel = i
			return
		}
	}
}

// finish runs what an answered question asked: val is "yes", the typed
// code, or "" when the user said no or cancelled.
func (m *model) finish(p prompt, val string) tea.Cmd {
	switch p {
	case promptFilter:
		m.filter, m.sel = val, 0
		if val != "" {
			sphere, state := m.sphere, "active"
			if m.archived {
				state = "archived"
			}
			return m.startSearch(val, sphere, state)
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
		return m.job("save "+e.name+" as a new version", func() doneMsg {
			r, err := m.st.Revise(e.id, e.path)
			if err != nil {
				return doneMsg{err: err}
			}
			e.seen, e.sum = e.mtime(), sumFile(e.path)
			return doneMsg{status: fmt.Sprintf("saved as version v%d", r.Version), reload: true}
		})
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
		id := m.target
		return m.job("seal "+shortID(id), func() doneMsg {
			r, err := m.st.Seal(id)
			if err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{status: fmt.Sprintf("sealed; %d clear version(s) erased", r.VersionsErased), reload: true}
		})
	case promptErase:
		plan := m.plan
		m.plan = nil
		if !strings.EqualFold(val, plan.Code) {
			m.setStatus("erasure cancelled: the code was not typed", false)
			return nil
		}
		return m.job("erase", func() doneMsg {
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
		})
	}
	return nil
}
