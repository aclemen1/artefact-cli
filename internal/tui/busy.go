package tui

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Background jobs show in the header through tuikit.Busy; ! lists them.

// ended keeps the outcome of the jobs ended in their command, read in Update.
type ended struct {
	mu   sync.Mutex
	list []doneMsg
}

func (e *ended) push(d doneMsg) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, d)
}

func (e *ended) take() []doneMsg {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.list
	e.list = nil
	return l
}

// job runs f as a background job under label: its status is the end text,
// its error the failure; its reload and back apply once it ends.
func (m *model) job(label string, f func() doneMsg) tea.Cmd {
	q := m.ended
	return m.busy.Run(label, func() (string, error) {
		d := f()
		q.push(d)
		return d.status, d.err
	})
}

// afterJobs applies what the ended jobs ask: back to the list, a reload.
func (m *model) afterJobs() tea.Cmd {
	var cmds []tea.Cmd
	for _, d := range m.ended.take() {
		cmds = append(cmds, m.onDone(d, false)) // Busy shows the status and the error
	}
	return tea.Batch(cmds...)
}

// failure is the error a loaded message carries, nil when it loaded well.
type failure interface{ failure() error }

func (msg listMsg) failure() error   { return msg.err }
func (msg detailMsg) failure() error { return msg.err }
func (msg plansMsg) failure() error  { return msg.err }
func (msg planMsg) failure() error   { return msg.err }
func (msg searchMsg) failure() error { return msg.err }

// failedMsg is a loaded message that failed: Busy records the error, the
// view takes the message as usual, without a status.
type failedMsg struct {
	inner tea.Msg
	err   error
}

func (f failedMsg) Error() string { return f.err.Error() }

// wrap follows a load as a background job under label.
func (m *model) wrap(label string, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return m.busy.Wrap(label, func() tea.Msg {
		msg := cmd()
		if f, ok := msg.(failure); ok && f.failure() != nil {
			return failedMsg{msg, f.failure()}
		}
		return msg
	})
}

// openJobs shows the recent jobs (tuikit.BusyKey).
func (m *model) openJobs() tea.Cmd {
	return m.openModal("Jobs", m.busy.List())
}

// onDone reports a finished action (when report) and reloads what it changed.
func (m *model) onDone(msg doneMsg, report bool) tea.Cmd {
	switch {
	case !report:
	case msg.err != nil:
		m.setStatus(msg.err.Error(), true)
	default:
		m.setStatus(msg.status, false)
	}
	var cmds []tea.Cmd
	if msg.back {
		m.mode, m.detail = modeList, nil
	}
	if msg.reload {
		cmds = append(cmds, m.load())
		if m.planView != nil {
			cmds = append(cmds, m.loadPlans())
		}
		if m.mode == modeDetail && m.detail != nil {
			cmds = append(cmds, m.loadDetail(m.detail.Card.ID))
		}
	}
	return tea.Batch(cmds...)
}

func archiveLabel(toArchive bool, title string) string {
	if toArchive {
		return "archive " + title
	}
	return "make active " + title
}

// sphereSuffix names the sphere a job is limited to.
func sphereSuffix(sphere string) string {
	if sphere == "" {
		return ""
	}
	return " " + sphere
}
