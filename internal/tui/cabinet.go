package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/aclemen1/tuikit"

	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// The shared keys of the TUIs of the office: the focus written at each
// selection, i (a line for the greffier), @ (the CABINET pane), u (undo the
// last write on the selection), and the Journal of the detail. Each runs a
// command of the configuration; a key whose command is missing says so.

// runExternal runs argv with stdin and the actor of the TUI handed on
// (<TOOL>_BY of the command, OFFICE_ACTOR); out is stdout even on a failure
// (the JSON envelope of the error). Tests replace it.
var runExternal = func(argv []string, stdin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), actor.Env(filepath.Base(argv[0]), actor.TUI)...)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		return nil, fmt.Errorf("%s: %v: %s", argv[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), err
}

// expand fills {ref}, {id}, {sphere} and {limit} of a command of the configuration.
func expand(argv []string, ref, sphere string, limit int) []string {
	out := make([]string, len(argv))
	r := strings.NewReplacer("{ref}", ref, "{id}", strings.TrimPrefix(ref, "artefact:"), "{sphere}", sphere, "{limit}", strconv.Itoa(limit))
	for i, a := range argv {
		out[i] = r.Replace(config.Expand(a))
	}
	return out
}

// envelope is the JSON answer of a tool of the office.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func readEnvelope(out []byte, err error) (envelope, error) {
	var env envelope
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &env); jerr != nil {
		if err != nil {
			return env, err
		}
		return env, fmt.Errorf("unreadable answer: %.200s", out)
	}
	return env, nil
}

// refOf is how the other tools cite an artefact.
func refOf(it *store.ListItem) string { return "artefact:" + it.ID }

// selection is the ref and the sphere of the selected artefact, or empty.
func (m *model) selection() (string, string) {
	if m.mode == modeDetail && m.detail != nil && m.detail.Card != nil {
		return "artefact:" + m.detail.Card.ID, m.detail.Card.Sphere
	}
	if it := m.current(); it != nil {
		return refOf(it), it.Sphere
	}
	return "", ""
}

// syncFocus writes the focus when the selection changed (after tuikit.FocusDelay).
func (m *model) syncFocus() tea.Cmd {
	ref, title, sphere := "", "", ""
	if m.mode == modeDetail && m.detail != nil && m.detail.Card != nil {
		ref, title, sphere = "artefact:"+m.detail.Card.ID, m.detail.Card.Title, m.detail.Card.Sphere
	} else if it := m.current(); it != nil {
		ref, title, sphere = refOf(it), it.Title, it.Sphere
	}
	if ref == m.focusRef {
		return nil
	}
	m.focusRef = ref
	return m.focus.Set(ref, title, sphere)
}

// ------------------------------------------------------------------ i

// askGreffier opens the free text for the greffier (i).
func (m *model) askGreffier() tea.Cmd {
	if len(m.cfg.Greffier.Text) == 0 {
		m.setStatus("greffier: no command configured (greffier.text)", true)
		return nil
	}
	ref, sphere := m.selection()
	m.greffierFor = [2]string{ref, sphere}
	title := "For the greffier"
	if ref != "" {
		title += " · " + shortID(strings.TrimPrefix(ref, "artefact:"))
	}
	return m.openModal(title, tuikit.NewEditor("greffier", "Line for the greffier", ""))
}

// greffierAnswer is what greffier text answers.
type greffierAnswer struct {
	Handled  bool                        `json:"handled"`
	Doubts   []struct{ Question string } `json:"doubts"`
	Proposal struct {
		Class        string `json:"class"`
		Sphere       string `json:"sphere"`
		Dossier      string `json:"dossier"`
		DossierTitle string `json:"dossier_title"`
		Status       string `json:"status"`
		Error        string `json:"error"`
		Writes       []struct {
			Key string `json:"key"`
		} `json:"writes"`
	} `json:"proposal"`
}

// line is the answer in one line: the proposal, then the doubts.
func (a greffierAnswer) line() string {
	p := a.Proposal
	var parts []string
	for _, s := range []string{p.Class, p.Sphere, strings.TrimSpace(p.Dossier + " " + p.DossierTitle)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	var keys []string
	for _, w := range p.Writes {
		keys = append(keys, w.Key)
	}
	if len(keys) > 0 {
		parts = append(parts, strings.Join(keys, ", "))
	}
	s := "greffier: " + strings.Join(parts, " · ")
	if p.Status != "" {
		s += " (" + p.Status + ")"
	}
	if p.Error != "" {
		s += ": " + p.Error
	}
	for _, d := range a.Doubts {
		s += " · doubt: " + d.Question
	}
	return strings.Join(strings.Fields(s), " ")
}

// sendGreffier runs greffier.text with the text on stdin (Busy).
func (m *model) sendGreffier(text string) tea.Cmd {
	ref, sphere := m.greffierFor[0], m.greffierFor[1]
	m.greffierFor = [2]string{}
	text = strings.TrimSpace(text)
	if text == "" {
		m.setStatus("nothing for the greffier", false)
		return nil
	}
	argv := expand(m.cfg.Greffier.Text, ref, sphere, 0)
	return m.job("greffier", func() doneMsg {
		env, err := readEnvelope(runExternal(argv, text))
		if err != nil {
			return doneMsg{err: err}
		}
		if !env.OK {
			msg := "failed"
			if env.Error != nil {
				msg = env.Error.Message
			}
			return doneMsg{err: fmt.Errorf("greffier: %s", msg)}
		}
		var a greffierAnswer
		if err := json.Unmarshal(env.Result, &a); err != nil {
			return doneMsg{err: fmt.Errorf("greffier: unreadable answer: %v", err)}
		}
		return doneMsg{status: a.line(), warn: len(a.Doubts) > 0, say: true}
	})
}

// ------------------------------------------------------------------ @

// openCabinet runs cabinet.open (@).
func (m *model) openCabinet() tea.Cmd {
	if len(m.cfg.Cabinet.Open) == 0 {
		m.setStatus("CABINET: no command configured yet (cabinet.open)", true)
		return nil
	}
	ref, sphere := m.selection()
	argv := expand(m.cfg.Cabinet.Open, ref, sphere, 0)
	return m.job("CABINET", func() doneMsg {
		if _, err := runExternal(argv, ""); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "CABINET open"}
	})
}

// ------------------------------------------------------------------ u

// undoResult is what undo last answers, dry or not.
type undoResult struct {
	Entry struct {
		ID           string   `json:"id"`
		At           string   `json:"at"`
		Type         string   `json:"type"`
		Text         string   `json:"text"`
		By           string   `json:"by"`
		Undo         []string `json:"undo"`
		Irreversible bool     `json:"irreversible"`
	} `json:"entry"`
	Command []string `json:"command"`
}

// undoPreviewMsg carries the entry to confirm to the TUI.
type undoPreviewMsg struct {
	ref, sphere string
	res         undoResult
}

func notReversible(id string) string {
	return "cannot be undone from here: validate it in undo tui --select " + id
}

// runUndo runs a command of undo with --format json; kind is the kind of an error answered.
func runUndo(argv []string) (undoResult, string, error) {
	var r undoResult
	env, err := readEnvelope(runExternal(append(argv, "--format", "json"), ""))
	if err != nil {
		return r, "", err
	}
	if !env.OK {
		if env.Error == nil {
			return r, "error", fmt.Errorf("undo: failed")
		}
		return r, env.Error.Kind, fmt.Errorf("%s", env.Error.Message)
	}
	if err := json.Unmarshal(env.Result, &r); err != nil {
		return r, "", fmt.Errorf("undo: unreadable answer: %v", err)
	}
	return r, "", nil
}

// askUndo previews the last write on the selection (u), then asks to confirm it.
func (m *model) askUndo() tea.Cmd {
	if len(m.cfg.Undo.Preview) == 0 || len(m.cfg.Undo.Last) == 0 {
		m.setStatus("undo: no command configured (undo.preview, undo.last)", true)
		return nil
	}
	ref, sphere := m.selection()
	if ref == "" {
		m.setStatus("undo: no artefact selected", true)
		return nil
	}
	argv := expand(m.cfg.Undo.Preview, ref, sphere, 0)
	short := shortID(strings.TrimPrefix(ref, "artefact:"))
	return m.busy.Wrap("undoable on "+short, func() tea.Msg {
		r, kind, err := runUndo(argv)
		switch {
		case kind == "not_found":
			return doneMsg{status: "nothing to undo on " + short}
		case err != nil:
			return failedMsg{doneMsg{err: err}, err}
		}
		return undoPreviewMsg{ref: ref, sphere: sphere, res: r}
	})
}

// confirmUndo shows the entry and its reverse; an irreversible reverse is never run from here.
func (m *model) confirmUndo(p undoPreviewMsg) tea.Cmd {
	e := p.res.Entry
	if e.Irreversible {
		m.setStatus(notReversible(e.ID), true)
		return nil
	}
	inverse := p.res.Command
	if len(inverse) == 0 {
		inverse = e.Undo
	}
	text := strings.Join(strings.Fields(e.Text), " ")
	q := fmt.Sprintf("Undo “%s”?\n\n%s · %s · %s\nreverse: %s", text, e.Type, e.By, e.At, strings.Join(inverse, " "))
	m.undoing = &p
	return m.openModal("Undo", tuikit.NewConfirm("undo", q))
}

// doUndo runs undo.last once confirmed.
func (m *model) doUndo() tea.Cmd {
	p := m.undoing
	m.undoing = nil
	if p == nil {
		return nil
	}
	argv := expand(m.cfg.Undo.Last, p.ref, p.sphere, 0)
	id := p.res.Entry.ID
	return m.job("undo on "+shortID(strings.TrimPrefix(p.ref, "artefact:")), func() doneMsg {
		r, kind, err := runUndo(argv)
		switch {
		case kind == "needs_validation":
			return doneMsg{status: notReversible(id), warn: true, say: true}
		case err != nil:
			return doneMsg{err: err}
		}
		return doneMsg{status: "undone: " + strings.Join(strings.Fields(r.Entry.Text), " "), reload: true, say: true}
	})
}

// ------------------------------------------------------------------ journal

// historyEntry is an entry of logbook ls --format json.
type historyEntry struct {
	ID   string   `json:"id,omitempty"`
	At   string   `json:"at"`
	Type string   `json:"type,omitempty"`
	Text string   `json:"text"`
	By   string   `json:"by,omitempty"`
	Undo []string `json:"undo,omitempty"`
}

// historyOf reads the journal of the artefact id; nil without history.ls, or on a failure.
func historyOf(cfg *config.Config, id, sphere string) ([]historyEntry, error) {
	if len(cfg.History.Ls) == 0 {
		return nil, nil
	}
	limit := cfg.History.Limit
	if limit <= 0 {
		limit = 10
	}
	env, err := readEnvelope(runExternal(expand(cfg.History.Ls, "artefact:"+id, sphere, limit), ""))
	if err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, fmt.Errorf("history.ls failed")
	}
	var res struct {
		Items []historyEntry `json:"items"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("history.ls: unreadable answer: %v", err)
	}
	return res.Items, nil
}

// journalLines is the Journal section of the detail, hidden when empty.
func (m *model) journalLines() []string {
	if len(m.journal) == 0 {
		return nil
	}
	undoable := len(m.cfg.Undo.Last) > 0
	lines := []string{"", sBold.Render(fmt.Sprintf("Journal (%d)", len(m.journal)))}
	for _, e := range m.journal {
		head := strings.TrimSpace(e.At + "  " + e.Type + "  " + e.By)
		mark := ""
		if undoable && len(e.Undo) > 0 {
			mark = "  " + sWarn.Render("↶ undoable")
		}
		lines = append(lines, sMuted.Render("  "+head)+mark)
		for _, l := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
			lines = append(lines, sText.Render("    "+l))
		}
	}
	return lines
}

// sharedKey runs i, @ and u, from any view but an input; ok is false for other keys.
func (m *model) sharedKey(key string) (tea.Cmd, bool) {
	switch key {
	case "i":
		return m.askGreffier(), true
	case "@":
		return m.openCabinet(), true
	case "u":
		return m.askUndo(), true
	}
	return nil, false
}
