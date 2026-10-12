package actions

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/journal"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// ByVar is artefact's own actor variable.
const ByVar = "ARTEFACT_BY"

// JournalStart runs the journal command; tests replace it.
var JournalStart = journal.Start

// Request is the actor request of a call: --by (or the caller's header), the
// server's declared actor, the TUI, a terminal on stdin.
func Request(ctx *spec.Context, tui bool) actor.Request {
	r := actor.Request{Var: ByVar, TUI: tui, Terminal: ctx.Terminal && ctx.Access == nil}
	if v, ok := ctx.Args["by"].(string); ok {
		r.Flag = v
	}
	if ctx.Access != nil {
		if r.Flag == "" {
			r.Flag = ctx.Access.By
		}
		r.Declared = ctx.Access.Declared
	}
	return r
}

// sign resolves the actor of a call into ctx.By. A write without actor is
// signed actor.Unknown with a warning and an unsigned journal entry through
// LastUnsignedDay, refused after; a read never warns.
func sign(a *spec.Action, ctx *spec.Context) error {
	if ctx.By != "" {
		return nil
	}
	req := Request(ctx, false)
	if !a.Signed {
		req.Flag = "" // by names a holder there
	}
	got, err := actor.Resolve(req)
	if a.Right != "write" && a.Right != "erase" {
		if err != nil || got.Unsigned {
			got.By = actor.Unknown
		}
		ctx.By = got.By
		return nil
	}
	if err != nil {
		return spec.UserError("%s", err.Error())
	}
	ctx.By = got.By
	if got.Unsigned {
		ctx.Warn(actor.Warning(ByVar))
		Unsigned(ctx, a)
	}
	return nil
}

// Unsigned records a write without actor through the journal command of the
// configuration, if any; a failure is a warning.
func Unsigned(ctx *spec.Context, a *spec.Action) {
	cfg, err := config.Load(ctx.Config)
	if err != nil || len(cfg.Journal.Command) == 0 {
		return
	}
	sphere := ctx.Str("sphere")
	if sphere == "" && len(cfg.Spheres) > 0 {
		sphere = cfg.Spheres[0]
	}
	e := journal.Entry{ID: "cli", Type: "unsigned", Sphere: sphere, By: actor.Unknown, At: actor.Now(), Text: actor.UnsignedText("artefact", words(ctx, a))}
	if err := JournalStart(cfg.Journal.Command, e); err != nil {
		ctx.Warn(err.Error())
	}
}

// words are the CLI words of the call, or the action and its arguments when
// it comes through the server or MCP.
func words(ctx *spec.Context, a *spec.Action) []string {
	if len(ctx.Argv) > 0 {
		return ctx.Argv
	}
	out := []string{a.Name}
	keys := make([]string, 0, len(ctx.Args))
	for k := range ctx.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := ctx.Args[k].(type) {
		case []string:
			out = append(out, "--"+k, strings.Join(v, ","))
		default:
			out = append(out, "--"+k, fmt.Sprint(v))
		}
	}
	if ctx.Access != nil {
		out = append(out, "(via "+ctx.Access.Agent+")")
	}
	return out
}

// openAs opens the store for an action that loads its configuration itself.
func openAs(ctx *spec.Context, cfg *config.Config, spheres []string) (*store.Store, error) {
	s, err := store.Open(cfg, spheres, "artefact-cli/"+Version)
	if err != nil {
		return nil, err
	}
	s.By = ctx.By
	if s.By == "" {
		s.By = actor.Unknown
	}
	return s, nil
}
