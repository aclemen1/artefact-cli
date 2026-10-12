// Package actor resolves who signs a write: a gesture in a TUI is always
// the human; otherwise --by, the actor declared for a server or a daemon, the
// tool's own variable, OFFICE_ACTOR, or a terminal on stdin. Without any, a write is signed Unknown through
// LastUnsignedDay (Europe/Zurich), then refused.
package actor

import (
	"fmt"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Zurich without the system's zoneinfo
)

const (
	TUI     = "human:AC@tui"
	CLI     = "human:AC@cli"
	Unknown = "unknown:cli"
	// OfficeVar is posed by office in each dossier session and by routine at each run.
	OfficeVar = "OFFICE_ACTOR"
	// LastUnsignedDay is the last day, in Zone, a write without actor is accepted.
	LastUnsignedDay = "2026-10-19"
	Zone            = "Europe/Zurich"
)

// Now is the clock of the deadline; tests replace it.
var Now = time.Now

// Kinds an actor may take.
var Kinds = []string{"human", "agent", "routine"}

// Request is what the caller knows of the write.
type Request struct {
	Flag     string              // value of --by
	Var      string              // the tool's variable, e.g. ALARM_BY
	Declared string              // actor declared in configuration for a server or a daemon
	TUI      bool                // the write comes from a gesture in the TUI
	Terminal bool                // the CLI runs with stdin on a terminal
	Getenv   func(string) string // nil: os.Getenv
	Now      func() time.Time    // nil: Now
}

// Actor is the resolved signature.
type Actor struct {
	By       string `json:"by"`
	From     string `json:"from"` // flag, env:<VAR>, config, tui, terminal, none
	Unsigned bool   `json:"unsigned,omitempty"`
}

// Error is an actor refused or missing; the caller maps it to a user error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Resolve applies the order of the convention: the TUI, else --by, the
// declared actor, the tool variable, OFFICE_ACTOR, a terminal.
func Resolve(r Request) (Actor, error) {
	// A gesture at the keyboard of a TUI is always the human: an inherited
	// <TOOL>_BY or OFFICE_ACTOR does not sign it.
	if r.TUI {
		return Actor{By: TUI, From: "tui"}, nil
	}
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	type cand struct{ v, from string }
	cands := []cand{{r.Flag, "flag"}, {r.Declared, "config"}}
	if r.Var != "" {
		cands = append(cands, cand{getenv(r.Var), "env:" + r.Var})
	}
	cands = append(cands, cand{getenv(OfficeVar), "env:" + OfficeVar})
	for _, c := range cands {
		v := strings.TrimSpace(c.v)
		if v == "" {
			continue
		}
		if err := Check(v); err != nil {
			return Actor{}, &Error{fmt.Sprintf("%s (from %s)", err.Error(), c.from)}
		}
		return Actor{By: v, From: c.from}, nil
	}
	if r.Terminal {
		return Actor{By: CLI, From: "terminal"}, nil
	}
	now := r.Now
	if now == nil {
		now = Now
	}
	if Expired(now()) {
		return Actor{}, &Error{Missing(r.Var)}
	}
	return Actor{By: Unknown, From: "none", Unsigned: true}, nil
}

// Expired reports whether t is past LastUnsignedDay in Zone.
func Expired(t time.Time) bool {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		loc = time.FixedZone("CEST", 2*3600)
	}
	last, _ := time.ParseInLocation("2006-01-02", LastUnsignedDay, loc)
	return !t.Before(last.AddDate(0, 0, 1))
}

// Missing says how to pose the actor.
func Missing(v string) string {
	via := "--by <actor>"
	if v != "" {
		via += ", " + v
	}
	return "no actor for this write: pass " + via + " or " + OfficeVar +
		", e.g. --by human:AC@cli or --by agent:office-P-0019 (kinds: human, agent, routine)"
}

// Warning is the notice of a write signed Unknown.
func Warning(v string) string {
	return "write signed " + Unknown + ": " + Missing(v) + "; refused after " + LastUnsignedDay
}

// Check refuses an actor that is not <kind>:<id> with a known kind.
func Check(v string) error {
	if strings.ContainsAny(v, " \t\r\n") {
		return &Error{fmt.Sprintf("actor %q holds a blank; expected <kind>:<id>, e.g. human:AC@cli", v)}
	}
	kind, id, ok := strings.Cut(v, ":")
	if !ok || id == "" {
		return &Error{fmt.Sprintf("actor %q: expected <kind>:<id>, e.g. human:AC@cli or agent:office-P-0019", v)}
	}
	for _, k := range Kinds {
		if kind == k {
			return nil
		}
	}
	return &Error{fmt.Sprintf("actor %q: kind %q is not one of human, agent, routine; e.g. human:AC@cli", v, kind)}
}

// UnsignedText is the text of the unsigned entry: the tool and its command.
func UnsignedText(tool string, argv []string) string {
	cmd := strings.Join(append([]string{tool}, argv...), " ")
	if r := []rune(cmd); len(r) > 300 {
		cmd = string(r[:300]) + "…"
	}
	return fmt.Sprintf("Écriture sans acteur, signée %s.\noutil : %s\ncommande : %s\n", Unknown, tool, cmd)
}

// Env is the environment that hands the actor to a command another tool runs:
// <TOOL>_BY for that tool, and OFFICE_ACTOR.
func Env(tool, by string) []string {
	if by == "" {
		return nil
	}
	return []string{Var(tool) + "=" + by, OfficeVar + "=" + by}
}

// Var is the actor variable of a tool: alarm → ALARM_BY.
func Var(tool string) string {
	t := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(tool))
	return t + "_BY"
}
