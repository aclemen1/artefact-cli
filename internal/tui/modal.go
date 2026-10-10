package tui

import (
	"fmt"
	"github.com/aclemen1/artefact-cli/internal/notes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/aclemen1/tuikit"
	"gopkg.in/yaml.v3"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// Every input but the filter (/) opens in a centred modal from tuikit; the
// modal takes every key while it is open.

var modalPrompts = map[string]prompt{
	"seal": promptSeal, "erase": promptErase, "reveal": promptReveal, "revise": promptRevise,
}

func (m *model) openModal(title string, c tuikit.Content) tea.Cmd {
	m.modal = tuikit.NewModal(title, c).SetSize(m.w, m.h)
	return nil
}

// confirm asks a yes/no question; the answer goes to finish(kind, …).
func (m *model) confirm(kind prompt, id, title, question string) tea.Cmd {
	return m.openModal(title, tuikit.NewConfirm(id, question))
}

// onModal handles a modal's answer.
func (m *model) onModal(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tuikit.DoneMsg:
		m.modal = nil
		switch msg.ID {
		case "deposit":
			v := msg.Values
			date := ""
			if mo, ok := v.Moment("date"); ok {
				date = mo.Date
			}
			return m.deposit(v.String("path"), v.String("sphere"), v.String("title"), date), true
		case "card":
			return m.applyCardForm(msg.Values), true
		case "note":
			it := m.noteFor
			m.noteFor = nil
			body := strings.TrimSpace(msg.Values.String("note"))
			if it == nil || body == "" {
				m.setStatus("no note: it was empty", false)
				return nil, true
			}
			return m.job("note on "+it.Title, func() doneMsg {
				if err := notes.Add(m.cfg, it.ID, it.Sphere, body); err != nil {
					return doneMsg{err: err}
				}
				return doneMsg{status: "note added", reload: true}
			}), true
		case "erase":
			if m.plan == nil {
				return nil, true
			}
			return m.finish(promptErase, m.plan.Code), true
		}
		if p, ok := modalPrompts[msg.ID]; ok {
			return m.finish(p, "yes"), true
		}
		return nil, true
	case tuikit.CancelMsg:
		m.modal = nil
		switch msg.ID {
		case "erase":
			m.plan = nil
			m.setStatus("erasure cancelled; the code is left unused and expires", false)
			return nil, true
		case "deposit", "card", "note":
			m.editCard, m.noteFor = nil, nil
			m.setStatus("cancelled", false)
			return nil, true
		}
		if p, ok := modalPrompts[msg.ID]; ok {
			return m.finish(p, ""), true
		}
		return nil, true
	}
	return nil, false
}

// askDeposit opens the deposit form (c: new).
func (m *model) askDeposit() tea.Cmd {
	sphere := tuikit.Choice("sphere", "Sphere", m.spheres...).Required().
		Help("by the nature of the content: a UNISIS or UNIL document goes to pro")
	form := tuikit.NewForm("deposit",
		tuikit.Text("path", "File").Required().Placeholder("~/Downloads/scan.pdf"),
		sphere,
		tuikit.Text("title", "Title").Help("defaults to the file name"),
		tuikit.Date("date", "Date").Help("date of the content; defaults to the file's own, else today"),
	).Validate(func(v tuikit.Values) tuikit.Errors {
		p := config.Expand(strings.TrimSpace(v.String("path")))
		if p == "" {
			return nil
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return tuikit.Errors{"path": "no such file"}
		}
		return nil
	})
	return m.openModal("Deposit a file", form)
}

// deposit puts the file into sphere.
func (m *model) deposit(path, sphere, title, date string) tea.Cmd {
	path = config.Expand(strings.TrimSpace(path))
	if !slices.Contains(m.spheres, sphere) {
		m.setStatus("sphere "+sphere+" is not one of "+strings.Join(m.spheres, ", "), true)
		return nil
	}
	return m.job("put "+filepath.Base(path)+" in "+sphere, func() doneMsg {
		f, err := os.Open(path)
		if err != nil {
			return doneMsg{err: err}
		}
		defer f.Close()
		r, err := m.st.Put(f, store.PutOptions{Sphere: sphere, Filename: filepath.Base(path), Title: strings.TrimSpace(title), Date: date})
		if err != nil {
			return doneMsg{err: err}
		}
		if r.Status == "known" {
			return doneMsg{status: "already kept: " + r.Title + " (" + shortID(r.ID) + ")", reload: true}
		}
		return doneMsg{status: "deposited in " + sphere + ": " + r.Title + " (" + shortID(r.ID) + ")", reload: true}
	})
}

// edit opens the card in a form (E).
func (m *model) edit(c *store.Card) tea.Cmd {
	m.editCard = c
	yamlOf := func(v map[string]any) string {
		if len(v) == 0 {
			return ""
		}
		b, _ := yaml.Marshal(v)
		return strings.TrimRight(string(b), "\n")
	}
	date := tuikit.Date("date", "Date").Help("date of the content")
	if c.Date != "" {
		date = date.Default(c.Date)
	}
	form := tuikit.NewForm("card",
		tuikit.Text("title", "Title").Required().Default(c.Title),
		date,
		tuikit.Text("filename", "File name").Default(c.Filename),
		tuikit.Text("mime", "Type").Default(c.Mime).Help("media type, e.g. application/pdf"),
		tuikit.Refs("tags", "Tags", m.tagCompleter(c.Sphere)).Default(append([]string(nil), c.Tags...)).
			Help("enter or comma adds one; backspace on an empty entry removes the last"),
		tuikit.TextArea("dc", "Dublin Core").Default(yamlOf(c.DC)).Help("YAML: creator: …, subject: …"),
		tuikit.TextArea("x", "Extensions").Default(yamlOf(c.X)).Help("YAML, one namespace per key"),
	).Validate(func(v tuikit.Values) tuikit.Errors {
		errs := tuikit.Errors{}
		for _, k := range []string{"dc", "x"} {
			var any map[string]any
			if err := yaml.Unmarshal([]byte(v.String(k)), &any); err != nil {
				errs[k] = "not YAML: " + err.Error()
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return errs
	})
	return m.openModal("Card of "+c.Title, form)
}

// applyCardForm saves what the card form changed.
func (m *model) applyCardForm(v tuikit.Values) tea.Cmd {
	cur := m.editCard
	m.editCard = nil
	if cur == nil {
		return nil
	}
	after := editable{Title: strings.TrimSpace(v.String("title")), Filename: strings.TrimSpace(v.String("filename")),
		Mime: strings.TrimSpace(v.String("mime"))}
	if mo, ok := v.Moment("date"); ok {
		after.Date = mo.Date
	}
	for _, t := range v.Strings("tags") {
		if t = strings.TrimSpace(t); t != "" {
			after.Tags = append(after.Tags, t)
		}
	}
	if err := yaml.Unmarshal([]byte(v.String("dc")), &after.DC); err != nil {
		m.setStatus("card not changed: dc: "+err.Error(), true)
		return nil
	}
	if err := yaml.Unmarshal([]byte(v.String("x")), &after.X); err != nil {
		m.setStatus("card not changed: x: "+err.Error(), true)
		return nil
	}
	return m.saveCard(cur, after)
}

func eraseQuestion(p *store.Plan) string {
	var b strings.Builder
	b.WriteString("Erase for good, everywhere:\n")
	for _, it := range p.Items {
		what := "the whole artefact"
		if !it.Whole {
			what = "versions " + strings.Join(it.Versions, ", ")
		}
		fmt.Fprintf(&b, "  • %s: %s; %d location(s)", it.Title, what, len(it.Locations))
		if len(it.Holders) > 0 {
			fmt.Fprintf(&b, ", still held by %d", len(it.Holders))
		}
		b.WriteString("\n")
	}
	b.WriteString("Type the code to confirm.")
	return b.String()
}

// tagCompleter proposes the tags already used in sphere, most used first.
func (m *model) tagCompleter(sphere string) tuikit.Completer {
	counts, _ := m.st.TagCounts(sphere)
	tags := make([]string, 0, len(counts))
	for t := range counts {
		tags = append(tags, t)
	}
	slices.SortFunc(tags, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	return func(q string) []tuikit.Item {
		q = strings.ToLower(strings.TrimSpace(q))
		var out []tuikit.Item
		for _, t := range tags {
			if q == "" || strings.Contains(strings.ToLower(t), q) {
				out = append(out, tuikit.Item{Value: t, Label: fmt.Sprintf("%s (%d)", t, counts[t])})
			}
		}
		return out
	}
}
