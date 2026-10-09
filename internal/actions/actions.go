// Package actions registers every action of artefact in the spec registry.
package actions

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"github.com/aclemen1/artefact-cli/internal/notes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/keys"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
	"golang.org/x/term"
)

const Version = "0.1.0-dev"

//go:embed skill.md
var skillText string

func open(ctx *spec.Context) (*store.Store, error) {
	cfg, err := config.Load(ctx.Config)
	if err != nil {
		return nil, spec.UserError("%v", err)
	}
	agent := os.Getenv("ARTEFACT_AGENT")
	if agent == "" {
		agent = "artefact-cli/" + Version
	}
	var spheres []string
	if ctx.Access != nil {
		spheres, agent = ctx.Access.Spheres, ctx.Access.Agent
	}
	return store.Open(cfg, spheres, agent)
}

// wrap maps store errors onto the envelope's kinds.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var ue *store.UserError
	var nf *store.NotFound
	switch {
	case errors.As(err, &ue):
		return spec.UserError("%s", ue.Msg)
	case errors.As(err, &nf):
		return spec.NotFound("%s", nf.Msg)
	}
	return err
}

func run(ctx *spec.Context, f func(*store.Store) (any, error)) (any, error) {
	s, err := open(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	defer s.Close()
	res, err := f(s)
	return res, wrap(err)
}

func parseHolds(list []string) ([]index.Holder, error) {
	var out []index.Holder
	for _, h := range list {
		by, ref, _ := strings.Cut(h, ":")
		if by == "" {
			return nil, spec.UserError("--hold takes <by>:<ref>, got %q. Example: --hold mnemo:01K6Z8Q4V7XW3D9JH2M5N8P0QR", h)
		}
		out = append(out, index.Holder{By: by, Ref: ref})
	}
	return out, nil
}

func init() {
	// ---------------------------------------------------------------- artefacts
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "put", Right: "write",
		Summary:    "Deposit a file as an artefact, or find the artefact that already holds it.",
		Discussion: "Idempotent within the sphere: a payload already stored returns its artefact with status known, plus the holders recorded on it. --key makes a retried deposit return the first result. A deposit is atomic: an interrupted put leaves nothing.",
		Params: []spec.Param{
			{Name: "file", Kind: spec.String, Positional: true, Required: true, Help: "File to deposit, or - to read the payload from standard input."},
			{Name: "sphere", Kind: spec.String, Required: true, Help: "Sphere to deposit into, chosen by the nature of the content (a UNISIS document goes to pro); there is no default.",
				Missing: "put needs --sphere: the sphere follows the content (a UNISIS document goes to pro), never a default. Example: artefact put scan.pdf --sphere pro"},
			{Name: "filename", Kind: spec.String, Help: "File name to record, for a payload read from standard input (sets the extension)."},
			{Name: "title", Kind: spec.String, Help: "Title of the card. Defaults to the file name without extension."},
			{Name: "date", Kind: spec.String, Help: "Date of the content, YYYY-MM-DD. Defaults to today."},
			{Name: "mime", Kind: spec.String, Help: "Media type. Defaults to the one of the extension, then to the content's."},
			{Name: "hold", Kind: spec.StringList, Help: "Holder to record, <by>:<ref> (repeatable)."},
			{Name: "card", Kind: spec.String, Help: "YAML of editable card fields to merge: title, date, filename, mime, dc, x."},
			{Name: "key", Kind: spec.String, Help: "Idempotency key, scoped to the sphere: a second put with this key and the same payload returns the first result."},
			{Name: "seal", Kind: spec.Bool, Help: "Encrypt the payload with the sphere's age key before anything is written."},
			{Name: "render", Kind: spec.Bool, Help: "Make the text rendition with lector before answering (unsealed artefacts only)."},
		},
		Examples: []string{
			"artefact put ordonnance-2026-09.pdf --sphere perso",
			"artefact put facture.pdf --sphere perso --render",
			"artefact put scan.png --sphere perso --hold mnemo:01K6Z8Q4V7XW3D9JH2M5N8P0QR --key 01K6Z8Q4V7XW3D9JH2M5N8P0QR",
			"cat note.txt | artefact put - --sphere pro --filename note.txt --title \"Note de séance\" --date 2026-09-18",
			"artefact put facture.pdf --sphere perso --card 'dc: {creator: Romande Energie}'",
			"artefact put codes-2fa.png --sphere perso --seal",
		},
		Effects: []string{"Writes a new OCFL object (payload, card, event log) into the sphere's store, unless the payload is known.", "Records the holders given with --hold."},
		Run: func(ctx *spec.Context) (any, error) {
			holds, err := parseHolds(ctx.List("hold"))
			if err != nil {
				return nil, err
			}
			return run(ctx, func(s *store.Store) (any, error) {
				o := store.PutOptions{Sphere: ctx.Str("sphere"), Filename: ctx.Str("filename"), Title: ctx.Str("title"),
					Date: ctx.Str("date"), Mime: ctx.Str("mime"), Holds: holds, Card: ctx.Str("card"), Key: ctx.Str("key"), Seal: ctx.Bool("seal")}
				var r io.Reader
				if f := ctx.Str("file"); f != "-" && !localPaths(ctx) {
					return nil, spec.UserError("this server does not read local paths; send the payload itself (file \"-\")")
				} else if f == "-" {
					r = ctx.Stdin
				} else {
					fh, err := os.Open(config.Expand(f))
					if err != nil {
						return nil, spec.UserError("cannot read %q: %v. Example: artefact put ~/Downloads/scan.pdf", f, err)
					}
					defer fh.Close()
					if fi, _ := fh.Stat(); fi != nil && fi.IsDir() {
						return nil, spec.UserError("%q is a folder; put takes one file. Example: artefact put %s/scan.pdf", f, f)
					}
					if o.Filename == "" {
						o.Filename = filepath.Base(f)
					}
					r = fh
				}
				res, err := s.Put(r, o)
				if err != nil || !ctx.Bool("render") || res.Sealed {
					return res, err
				}
				res.Render, err = s.Render(res.URI, false)
				return res, err
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "get", Right: "read",
		Summary: "Write an artefact's payload to standard output or to a file.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id (ULID) or URI artefact://<sphere>/<id>."},
			{Name: "version", Kind: spec.String, Help: "Version to read, e.g. v1. Defaults to the current one."},
			{Name: "out", Kind: spec.String, Help: "File to write, with mode 0600; the result then reports it. Without it, the raw bytes go to standard output."},
			{Name: "reveal", Kind: spec.Bool, Help: "Decrypt a sealed artefact; the clear text exists only where it is written."},
			{Name: "rendition", Kind: spec.String, Help: "Read a rendition instead of the payload: text (Markdown made by lector)."},
		},
		Examples: []string{"artefact get 01K9B3T0Z6Q8M2C4R7V1X5N9PA --out /tmp/task/scan.pdf", "artefact get 01K9B3T0Z6Q8M2C4R7V1X5N9PA --rendition text", "artefact get artefact://perso/01K9B3T0Z6Q8M2C4R7V1X5N9PA > scan.pdf", "artefact get 01K9B3T0Z6Q8M2C4R7V1X5N9PA --version v1 --out v1.pdf", "artefact get 01K9B3T0Z6Q8M2C4R7V1X5N9PA --reveal --out /tmp/task/codes.png"},
		Effects:  []string{"Reads only. With --out, creates or replaces that file."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				if ctx.Bool("reveal") && !ctx.Access.Can("reveal") {
					return nil, spec.Forbidden("this token has no reveal right; a sealed artefact stays sealed")
				}
				if ctx.Str("out") != "" && !localPaths(ctx) {
					return nil, spec.UserError("this server does not write local files; read the payload without --out")
				}
				var rc io.ReadCloser
				var c *store.Card
				var err error
				switch ctx.Str("rendition") {
				case "":
					rc, c, err = s.OpenPayload(ctx.Str("id"), ctx.Str("version"), ctx.Bool("reveal"))
				case "text":
					if ctx.Str("version") != "" {
						return nil, spec.UserError("a rendition exists for the current version only; drop --version")
					}
					var t string
					t, c, err = s.RenditionText(ctx.Str("id"))
					rc = io.NopCloser(strings.NewReader(t))
				default:
					return nil, spec.UserError("--rendition takes text, got %q", ctx.Str("rendition"))
				}
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				out := ctx.Str("out")
				if out == "" {
					if _, err := io.Copy(ctx.Stdout, rc); err != nil {
						return nil, err
					}
					return spec.Streamed{}, nil
				}
				out = config.Expand(out)
				f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return nil, spec.UserError("cannot write %q: %v", out, err)
				}
				n, err := io.Copy(f, rc)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return nil, err
				}
				return map[string]any{"id": c.ID, "out": out, "size": n, "mime": c.Mime}, nil
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "stat", Right: "read",
		Summary:    "Describe artefacts: card, versions, holders, locations.",
		Discussion: "Takes one or many ids in one call; ids that match no artefact are listed under missing, not reported as an error.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.StringList, Positional: true, Required: true, Help: "Artefact ids or URIs (several allowed)."},
			{Name: "with-events", Kind: spec.Bool, Help: "Include each artefact's event log."},
			{Name: "with-notes", Kind: spec.Bool, Help: "Include each artefact's notes, read by the notes.ls command of the configuration (one call per artefact)."},
		},
		Examples: []string{"artefact stat 01K9B3T0Z6Q8M2C4R7V1X5N9PA", "artefact stat 01K9B3T0Z6Q8M2C4R7V1X5N9PA 01K9B3V2A8C0E2G4J6L8N0Q2S4 --with-events"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				st, missing, err := s.Stat(ctx.List("id"), ctx.Bool("with-events"))
				if err != nil {
					return nil, err
				}
				if ctx.Bool("with-notes") {
					for i := range st {
						if st[i].Notes, err = notes.Of(s.Cfg, st[i].Card.ID, st[i].Card.Sphere); err != nil {
							return nil, err
						}
					}
				}
				return map[string]any{"artefacts": st, "missing": missing}, nil
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "ls", Right: "read",
		Summary: "List artefacts, newest deposit first.",
		Params: []spec.Param{
			{Name: "sphere", Kind: spec.String, Help: "List this sphere only. Defaults to every granted sphere."},
			{Name: "held-by", Kind: spec.String, Help: "Keep the artefacts held by this tool, or by tool:ref."},
			{Name: "orphaned", Kind: spec.Bool, Help: "Keep the artefacts that no one holds."},
			{Name: "class", Kind: spec.String, Help: "Keep the artefacts placed in plan, or in plan:notation and below."},
			{Name: "pending", Kind: spec.Bool, Help: "Keep the artefacts whose classification waits for a human (in --class's plan when given)."},
			{Name: "state", Kind: spec.String, Default: "active", Enum: []string{"active", "archived", "all"}, Help: "active hides the archived artefacts; archived shows them only; all shows both."},
			{Name: "tag", Kind: spec.String, Help: "Keep the artefacts with this tag."},
			{Name: "since", Kind: spec.String, Help: "Keep the artefacts deposited on or after this date (YYYY-MM-DD)."},
			{Name: "limit", Kind: spec.String, Default: "100", Help: "Maximum number of artefacts; 0 for all.", Aliases: []string{"n"}},
			{Name: "sort", Kind: spec.String, Help: "Order: date, deposited (default), updated, title or size, with :asc or :desc. Dates and sizes default to newest and largest first, titles to A-Z."},
		},
		Examples: []string{"artefact ls --sphere perso", "artefact ls --sort date:asc", "artefact ls --sort title", "artefact ls --held-by mnemo --limit 0", "artefact ls --held-by mnemo:01K6Z8Q4V7XW3D9JH2M5N8P0QR", "artefact ls --orphaned"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			limit, err := strconv.Atoi(ctx.Str("limit"))
			if err != nil || limit < 0 {
				return nil, spec.UserError("--limit takes a number, got %q. Example: artefact ls --limit 20", ctx.Str("limit"))
			}
			order, err := store.ParseSort(ctx.Str("sort"))
			if err != nil {
				return nil, wrap(err)
			}
			return run(ctx, func(s *store.Store) (any, error) {
				return s.List(ctx.Str("sphere"), index.Filter{HeldBy: ctx.Str("held-by"), Class: ctx.Str("class"), Pending: ctx.Bool("pending"), State: stateFilter(ctx.Str("state")), Tag: ctx.Str("tag"), Orphaned: ctx.Bool("orphaned"), Since: ctx.Str("since"), SortKey: order.Key, SortDesc: order.Desc, Limit: limit})
			})
		},
		Text: func(w io.Writer, r any) {
			for _, it := range r.([]store.ListItem) {
				fmt.Fprintf(w, "%s  %-6s %s  %s  %s\n", it.ID, it.Sphere, it.Date, it.Mime, it.Title)
			}
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "card", Right: "write",
		Summary:    "Edit an artefact's card: title, date, filename, mime, Dublin Core terms, extensions.",
		Discussion: "--set dc.<term>= or x.<namespace>.<key>= with an empty value removes it. --merge merges dc terms one by one and replaces x namespaces whole. The core fields (id, sphere, digest, size, version, holders) are written by artefact only.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."},
			{Name: "set", Kind: spec.StringList, Help: "<field>=<value> (repeatable)."},
			{Name: "merge", Kind: spec.String, Help: "YAML mapping of editable fields to merge."},
			{Name: "tag", Kind: spec.StringList, Help: "Tag to add (repeatable)."},
			{Name: "untag", Kind: spec.StringList, Help: "Tag to remove (repeatable)."},
		},
		Examples: []string{"artefact card 01K9B3T0Z6Q8M2C4R7V1X5N9PA --set title=\"Ordonnance septembre 2026\" --set date=2026-09-18", "artefact card 01K9B3T0Z6Q8M2C4R7V1X5N9PA --set dc.creator=\"Dr X\"", "artefact card 01K9B3T0Z6Q8M2C4R7V1X5N9PA --merge 'x: {mnemo: {note: pour la caisse AVS}}'", "artefact card 01K9B3T0Z6Q8M2C4R7V1X5N9PA --tag jeannette --tag avs --untag brouillon"},
		Effects:  []string{"Rewrites the card and appends a card event. Creates no version."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				if len(ctx.List("set")) == 0 && ctx.Str("merge") == "" {
					if len(ctx.List("tag")) > 0 || len(ctx.List("untag")) > 0 {
						return s.Tag(ctx.Str("id"), ctx.List("tag"), ctx.List("untag"))
					}
				}
				c, err := s.EditCard(ctx.Str("id"), ctx.List("set"), ctx.Str("merge"))
				if err != nil || (len(ctx.List("tag")) == 0 && len(ctx.List("untag")) == 0) {
					return c, err
				}
				return s.Tag(ctx.Str("id"), ctx.List("tag"), ctx.List("untag"))
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "hold", Right: "write",
		Summary:    "Record that a tool or person relies on an artefact.",
		Discussion: "Idempotent: holding twice with the same --by and --ref records one holder.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."},
			{Name: "by", Kind: spec.String, Required: true, Help: "Holding tool or person, e.g. mnemo or human:alain."},
			{Name: "ref", Kind: spec.String, Help: "What of the holder relies on it, e.g. a concept ULID."},
		},
		Examples: []string{"artefact hold 01K9B3T0Z6Q8M2C4R7V1X5N9PA --by mnemo --ref 01K6Z8Q4V7XW3D9JH2M5N8P0QR"},
		Effects:  []string{"Adds the holder to the card and appends a hold event, unless it is already there."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) { return s.Hold(ctx.Str("id"), ctx.Str("by"), ctx.Str("ref")) })
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "release", Right: "write",
		Summary:    "Withdraw a holder from an artefact.",
		Discussion: "Without --ref, removes every holder of --by. Releasing an absent holder succeeds with released 0. An artefact left without holders is not deleted; `artefact ls --orphaned` lists it.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."},
			{Name: "by", Kind: spec.String, Required: true, Help: "Holding tool or person."},
			{Name: "ref", Kind: spec.String, Help: "Only this reference of the holder."},
		},
		Examples: []string{"artefact release 01K9B3T0Z6Q8M2C4R7V1X5N9PA --by mnemo --ref 01K6Z8Q4V7XW3D9JH2M5N8P0QR", "artefact release 01K9B3T0Z6Q8M2C4R7V1X5N9PA --by mnemo"},
		Effects:  []string{"Removes the matching holders from the card and appends a release event."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				c, n, err := s.Release(ctx.Str("id"), ctx.Str("by"), ctx.Str("ref"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"released": n, "card": c}, nil
			})
		},
	})

	spec.Register(&spec.Action{
		Category: "artefacts", Name: "seal", Right: "write",
		Summary:     "Seal an artefact with its sphere's age key and erase its clear versions.",
		Discussion:  "The artefact keeps its id and holders and becomes a single sealed version; every clear version is erased from the store. Sealing a sealed artefact does nothing (status already_sealed).",
		Params:      []spec.Param{{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."}},
		Examples:    []string{"artefact seal 01K9B3T0Z6Q8M2C4R7V1X5N9PA"},
		Effects:     []string{"Rewrites the object: one sealed version; the clear versions are deleted from the store.", "Appends a seal event."},
		Destructive: true,
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) { return s.Seal(ctx.Str("id")) })
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "redact", Right: "write",
		Summary:    "Add a version made from a redacted copy of the payload; with --purge, erase the earlier versions.",
		Discussion: "The redacted file is copied into the store. A sealed artefact stays sealed: the new version is encrypted too.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."},
			{Name: "from", Kind: spec.String, Required: true, Help: "The redacted file."},
			{Name: "purge", Kind: spec.Bool, Help: "Erase every earlier version from the store."},
		},
		Examples:    []string{"artefact redact 01K9B3T0Z6Q8M2C4R7V1X5N9PA --from note-redacted.md", "artefact redact 01K9B3T0Z6Q8M2C4R7V1X5N9PA --from note-redacted.md --purge"},
		Effects:     []string{"Adds a version; with --purge, deletes the earlier versions from the store.", "Appends a redact event."},
		Destructive: true,
		Run: func(ctx *spec.Context) (any, error) {
			if !localPaths(ctx) {
				return nil, spec.UserError("redact reads --from on this machine; this server does not read local paths")
			}
			return run(ctx, func(s *store.Store) (any, error) { return s.Redact(ctx.Str("id"), ctx.Str("from"), ctx.Bool("purge")) })
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "erase", Right: "erase",
		Summary: "Erase artefacts, or one version, for good: first a plan and a code, then --confirm <code>.",
		Discussion: "Step 1 returns the plan, its digest and a single-use code, valid ten minutes by default (erase.code_ttl). In a terminal, the code is asked for at once; --plan returns it instead, for a caller that confirms through its own interface. " +
			"Step 2, --confirm <code>, plans again and refuses if anything changed, then deletes the objects and leaves a tombstone. The proof names no file and no title. A held artefact needs its holders to release it, or a human's --force.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.StringList, Positional: true, Help: "Artefact ids or URIs to erase."},
			{Name: "version", Kind: spec.String, Help: "Erase only this version of a single artefact, e.g. v1."},
			{Name: "force", Kind: spec.Bool, Help: "Erase despite holders. For a human only."},
			{Name: "plan", Kind: spec.Bool, Help: "Return the plan and the code without asking for it."},
			{Name: "confirm", Kind: spec.String, Help: "Code returned by the plan: runs the erasure."},
		},
		Examples: []string{
			"artefact erase 01K9B3T0Z6Q8M2C4R7V1X5N9PA --plan",
			"artefact erase --confirm K7QM-3XTP",
			"artefact erase 01K9B3T0Z6Q8M2C4R7V1X5N9PA --version v1 --plan",
		},
		Effects:     []string{"Plan: writes nothing but the pending plan, outside the store.", "Confirm: deletes the objects or versions from the store, removes them from the index, records a tombstone."},
		Destructive: true,
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				if code := ctx.Str("confirm"); code != "" {
					if len(ctx.List("id")) > 0 {
						return nil, spec.UserError("--confirm takes the code alone. Example: artefact erase --confirm K7QM-3XTP")
					}
					return s.ConfirmErase(code)
				}
				if len(ctx.List("id")) == 0 {
					return nil, spec.UserError("name the artefacts to erase. Example: artefact erase 01K9B3T0Z6Q8M2C4R7V1X5N9PA --plan")
				}
				if ctx.Bool("force") && ctx.Access != nil {
					return nil, spec.Forbidden("--force is for a human at the CLI; holders release their artefacts first")
				}
				plan, err := s.PlanErase(store.EraseRequest{Refs: ctx.List("id"), Version: ctx.Str("version"), Force: ctx.Bool("force")})
				if err != nil || ctx.Bool("plan") || ctx.Access != nil || !interactive() {
					return plan, err
				}
				printPlan(plan)
				fmt.Fprintf(os.Stderr, "Type the code %s to erase for good, anything else to cancel: ", plan.Code)
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if !strings.EqualFold(strings.TrimSpace(line), plan.Code) {
					return nil, spec.UserError("erasure cancelled: the code was not typed")
				}
				return s.ConfirmErase(plan.Code)
			})
		},
	})

	// ---------------------------------------------------------------- ops
	spec.Register(&spec.Action{
		Category: "ops", Name: "key",
		Summary:    "Create a sphere's age key: public key in the configuration, private key in the macOS keychain.",
		Discussion: "init is idempotent: when the keychain already holds the sphere's key, its public key is written back to the configuration and returned. generate prints a new identity and its recipient and stores nothing: give the identity as $ARTEFACT_AGE_IDENTITY_<SPHERE> to a throwaway store, which then needs no keychain.",
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Required: true, Enum: []string{"init", "generate"}, Help: "init, or generate"},
			{Name: "sphere", Kind: spec.String, Help: "Sphere of the key, for init."},
		},
		Examples: []string{"artefact key init --sphere perso", "artefact key generate"},
		Effects:  []string{"init: writes the identity into the login keychain (service artefact, account age-<sphere>) and the recipient into the configuration. generate: none."},
		Run: func(ctx *spec.Context) (any, error) {
			if ctx.Str("verb") == "generate" {
				id, rcp, err := keys.Generate()
				if err != nil {
					return nil, err
				}
				return map[string]any{"identity": id, "recipient": rcp}, nil
			}
			cfg, err := config.Load(ctx.Config)
			if err != nil {
				return nil, spec.UserError("%v", err)
			}
			sp := ctx.Str("sphere")
			if !slices.Contains(cfg.Spheres, sp) {
				return nil, spec.UserError("sphere %q is not configured; configured spheres: %s", sp, strings.Join(cfg.Spheres, ", "))
			}
			status := "existing"
			identity, err := keys.Keychain{}.Identity(sp)
			if err != nil {
				if identity, _, err = keys.Generate(); err != nil {
					return nil, err
				}
				if err := keys.Store(sp, identity); err != nil {
					return nil, fmt.Errorf("keychain: %w", err)
				}
				status = "created"
			}
			rcp, err := keys.Recipient(identity)
			if err != nil {
				return nil, err
			}
			if cfg.Keys == nil {
				cfg.Keys = map[string]string{}
			}
			cfg.Keys[sp] = rcp
			if err := cfg.Save(cfg.Path()); err != nil {
				return nil, err
			}
			return map[string]any{"sphere": sp, "recipient": rcp, "status": status}, nil
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "init",
		Summary: "Create the configuration (if absent), one OCFL store and one index per sphere.",
		Params: []spec.Param{
			{Name: "store", Kind: spec.String, Help: "Store directory, for a new configuration. Default ~/artefact."},
			{Name: "spheres", Kind: spec.String, Help: "Comma-separated spheres, for a new configuration. Default perso."},
		},
		Examples: []string{"artefact init", "artefact init --store ~/artefact --spheres perso,pro"},
		Effects:  []string{"Writes ~/.config/artefact/config.yaml when absent.", "Creates the storage roots and indexes; existing ones are kept."},
		Run: func(ctx *spec.Context) (any, error) {
			path := ctx.Config
			if path == "" {
				path = config.DefaultPath()
			}
			cfg, err := config.Load(path)
			created := false
			if err != nil {
				if _, statErr := os.Stat(config.Expand(path)); statErr == nil {
					return nil, spec.UserError("%v", err)
				}
				cfg = config.Default()
				if v := ctx.Str("store"); v != "" {
					cfg.Store = v
				}
				if v := ctx.Str("spheres"); v != "" {
					cfg.Spheres = strings.Split(v, ",")
				}
				if err := cfg.Validate(); err != nil {
					return nil, spec.UserError("%v", err)
				}
				if err := cfg.Save(path); err != nil {
					return nil, err
				}
				created = true
			}
			s, err := store.Open(cfg, nil, "artefact-cli/"+Version)
			if err != nil {
				return nil, wrap(err)
			}
			defer s.Close()
			if err := s.Init(); err != nil {
				return nil, err
			}
			return map[string]any{"config": config.Expand(path), "config_created": created, "store": cfg.StorePath(), "spheres": cfg.Spheres}, nil
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "reindex",
		Summary: "Rebuild the indexes from the store: cards, inventories and event logs.",
		Params: []spec.Param{
			{Name: "sphere", Kind: spec.String, Help: "Rebuild this sphere only."},
			{Name: "search-only", Kind: spec.Bool, Help: "Rebuild only the full-text search index; the store stays usable meanwhile."},
			{Name: "semantic", Kind: spec.Bool, Help: "Build or refresh the index by meaning (qmd, one index artefact-<sphere> over the text renditions); the first run computes every vector."},
		},
		Examples: []string{"artefact reindex", "artefact reindex --sphere perso", "artefact reindex --search-only", "artefact reindex --semantic"},
		Effects:  []string{"Replaces the content of the sphere's index; the store is not changed."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				if ctx.Bool("semantic") {
					if !s.SemanticEnabled() {
						return nil, spec.UserError("search by meaning needs qmd (https://github.com/tobi/qmd) and search.semantic not set to none")
					}
					spheres := s.Spheres
					if sp := ctx.Str("sphere"); sp != "" {
						spheres = []string{sp}
					}
					var out []*store.SemanticResult
					for _, sp := range spheres {
						r, err := s.RefreshSemantic(context.Background(), sp)
						if err != nil {
							return nil, err
						}
						out = append(out, r)
					}
					return out, nil
				}
				if ctx.Bool("search-only") {
					return s.RebuildSearch(ctx.Str("sphere"))
				}
				return s.Reindex(ctx.Str("sphere"))
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "du", Right: "read",
		Summary:  "Count artefacts and bytes per sphere.",
		Params:   []spec.Param{{Name: "sphere", Kind: spec.String, Help: "Count this sphere only."}},
		Examples: []string{"artefact du", "artefact du --sphere perso"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) { return s.Usage(ctx.Str("sphere")) })
		},
	})

	// ---------------------------------------------------------------- meta
	spec.Register(&spec.Action{
		Category: "meta", Name: "version", Summary: "Print the artefact version.", Meta: true,
		Examples: []string{"artefact version"},
		Run:      func(*spec.Context) (any, error) { return Version, nil },
	})
	spec.Register(&spec.Action{
		Category: "meta", Name: "schema", Summary: "Browse actions: catalog, category, or one action's full spec.", Meta: true,
		Params: []spec.Param{
			{Name: "category", Kind: spec.String, Positional: true, Help: "Category to list."},
			{Name: "action", Kind: spec.String, Positional: true, Help: "Action to describe."},
			{Name: "search", Kind: spec.String, Help: "Match actions across categories."},
		},
		Examples: []string{"artefact schema", "artefact schema artefacts", "artefact schema artefacts put", "artefact schema --search holder"},
		Run: func(ctx *spec.Context) (any, error) {
			if q := ctx.Str("search"); q != "" {
				return spec.Search(q), nil
			}
			cat, act := ctx.Str("category"), ctx.Str("action")
			switch {
			case cat == "":
				return spec.Catalog(), nil
			case act == "":
				l := spec.ActionsIn(cat)
				if len(l) == 0 {
					return nil, spec.NotFound("no category %q. Categories: %s", cat, strings.Join(spec.Categories(), ", "))
				}
				return l, nil
			}
			a := spec.Find(cat, act)
			if a == nil {
				return nil, spec.NotFound("no action %q in %q. Try `artefact schema %s`", act, cat, cat)
			}
			return spec.Leaf{Action: a, Usage: spec.Usage(a)}, nil
		},
		Text: spec.TextSchema,
	})
	spec.Register(&spec.Action{
		Category: "meta", Name: "skill", Summary: "Print the embedded agent skill, or install it for an agent harness.", Meta: true,
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Default: "show", Enum: []string{"show", "install"}, Help: "show or install"},
			{Name: "for", Kind: spec.String, Default: "claude", Help: "Harness to install for: claude."},
			{Name: "dir", Kind: spec.String, Help: "Install into this directory instead."},
		},
		Effects:  []string{"install: writes SKILL.md into ~/.claude/skills/artefact/ (or --dir)."},
		Examples: []string{"artefact skill show", "artefact skill install --for claude"},
		Run: func(ctx *spec.Context) (any, error) {
			if ctx.Str("verb") == "install" {
				if f := ctx.Str("for"); f != "claude" && f != "claude-code" {
					return nil, spec.UserError("--for takes claude, got %q. Example: artefact skill install --for claude", f)
				}
				dir := config.Expand(ctx.Str("dir"))
				if dir == "" {
					home, _ := os.UserHomeDir()
					dir = filepath.Join(home, ".claude", "skills", "artefact")
				}
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
				p := filepath.Join(dir, "SKILL.md")
				if err := os.WriteFile(p, []byte(skillText), 0o644); err != nil {
					return nil, err
				}
				return "installed " + p, nil
			}
			return strings.TrimSpace(skillText), nil
		},
	})
}

func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func printPlan(p *store.Plan) {
	fmt.Fprintln(os.Stderr, "Erasure plan:")
	for _, it := range p.Items {
		what := "whole artefact"
		if !it.Whole {
			what = "version " + strings.Join(it.Versions, ", ")
		}
		fmt.Fprintf(os.Stderr, "  %s  %q  %s", it.URI, it.Title, what)
		if len(it.Holders) > 0 {
			fmt.Fprintf(os.Stderr, "  (held by %d)", len(it.Holders))
		}
		fmt.Fprintln(os.Stderr)
	}
	fmt.Fprintf(os.Stderr, "Code valid until %s.\n", p.Expires)
}

func localPaths(ctx *spec.Context) bool { return ctx.Access == nil || ctx.Access.LocalPaths }

// Invoke runs an action for a remote caller, within its rights.
func Invoke(a *spec.Action, ctx *spec.Context) (any, error) {
	if ctx.Access != nil {
		if a.Right == "" {
			return nil, spec.Forbidden("%s is not available over the server; run `artefact %s` on the machine", a.Name, a.Name)
		}
		if !ctx.Access.Can(a.Right) {
			return nil, spec.Forbidden("this token lacks the %s right that %s needs", a.Right, a.Name)
		}
	}
	return a.Run(ctx)
}

func stateFilter(s string) string {
	if s == "all" {
		return ""
	}
	return s
}

func init() {
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "search", Right: "read",
		Summary: "Find artefacts with a Google-like grammar: words, \"phrases\", -exclusions, OR, fields and filters; ranked by words and by meaning.",
		Discussion: `Words match the start of words, accents and case ignored; title and file name weigh more than the content, searched in the text of a text payload or of its text rendition. When the sphere has an index by meaning (reindex --semantic), texts close in meaning rank too, even without the words; each hit says via words, meaning or both. A plain word that matches nothing is corrected to the closest indexed word (corrected says so).

Grammar:
  "bail à loyer"        these words, whole, in this order (strict)
  -brouillon            without this word; -"two words" for a phrase
  assura OR css         either word (strict)
  loyer AROUND(5) garantie   both words within 5 words (strict)
  title:  file:  text:  card:   the word in this field only (strict); title:"a phrase"
  type:pdf  type:image  type:text  type:audio  type:video  type:docx  type:image/png
  class:4.2  class:perso:4.2  class:banque   in this class or below; a label word finds its classes
  plan:perso  tag:eve  -tag:eve
  after:2024-03  before:2025  year:2023  date:2026-10   by the content's date
  is:sealed  is:archived  is:active  is:pending  is:orphaned  is:classified  is:unclassified  (-is: negates)
  sphere:perso
Filters alone list what passes them, newest deposit first. A sealed artefact is found by its card only, never by meaning.`,
		Params: []spec.Param{
			{Name: "query", Kind: spec.StringList, Positional: true, Required: true, Help: "Words, phrases and operators (see the grammar)."},
			{Name: "sphere", Kind: spec.String, Help: "Search this sphere only. Defaults to every granted sphere."},
			{Name: "limit", Kind: spec.String, Default: "20", Help: "Maximum number of results.", Aliases: []string{"n"}},
			{Name: "state", Kind: spec.String, Default: "all", Enum: []string{"active", "archived", "all"}, Help: "Which artefacts to search: active, archived or all; is: in the query wins."},
			{Name: "sort", Kind: spec.String, Help: "Order: relevance (default), date, deposited, updated, title or size, with :asc or :desc. The limit applies after the sort."},
			{Name: "meaning", Kind: spec.String, Default: "auto", Enum: []string{"auto", "on", "off"}, Help: "Rank by meaning too: auto when the sphere has its index, on to say when it has none, off for words only."},
		},
		Examples: []string{"artefact search décompte assura", "artefact search litige avec la régie", `artefact search "bail à loyer" -brouillon type:pdf after:2020`, "artefact search assura OR css class:santé year:2025", "artefact search is:pending type:image --sort date"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			limit, err := strconv.Atoi(ctx.Str("limit"))
			if err != nil || limit <= 0 {
				return nil, spec.UserError("--limit takes a positive number, got %q", ctx.Str("limit"))
			}
			order, err := store.ParseSort(ctx.Str("sort"))
			if err != nil {
				return nil, wrap(err)
			}
			var semantic *bool
			switch ctx.Str("meaning") {
			case "on":
				t := true
				semantic = &t
			case "off":
				f := false
				semantic = &f
			}
			return run(ctx, func(s *store.Store) (any, error) {
				return s.Find(strings.Join(ctx.List("query"), " "), store.FindOptions{Sphere: ctx.Str("sphere"), Limit: limit, State: ctx.Str("state"), Sort: order, Semantic: semantic})
			})
		},
		Text: func(w io.Writer, r any) {
			res := r.(*store.FindResult)
			if res.Corrected != "" {
				fmt.Fprintf(w, "searched for: %s\n", res.Corrected)
			}
			for _, n := range res.Notes {
				fmt.Fprintf(w, "note: %s\n", n)
			}
			for _, h := range res.Hits {
				fmt.Fprintf(w, "%s  %-6s %s  %-13s %s\n    %s\n", h.ID, h.Sphere, h.Date, h.Via, h.Title, strings.ReplaceAll(h.Snippet, "\n", " "))
			}
		},
	})
}

func init() {
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "which", Right: "read",
		Summary:    "Find, without writing anything, the artefact that holds exactly the content of a file.",
		Discussion: "Looks up the file's SHA-256 among clear artefacts, and the keyed digest of the sphere among sealed ones (this needs the sphere's age key, from the keychain or $ARTEFACT_AGE_IDENTITY_<SPHERE>; without it, notes says so). Returns found false when no artefact holds it. Nothing is created, unlike put.",
		Params: []spec.Param{
			{Name: "file", Kind: spec.String, Positional: true, Required: true, Help: "File to look up, or - for standard input."},
			{Name: "sphere", Kind: spec.String, Help: "Look in this sphere only. Defaults to every granted sphere."},
		},
		Examples: []string{"artefact which ~/offices/perso/inbox/facture.pdf", "artefact which scan.pdf --sphere perso", "cat note.md | artefact which -"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				var r io.Reader
				if f := ctx.Str("file"); f == "-" {
					r = ctx.Stdin
				} else {
					if !localPaths(ctx) {
						return nil, spec.UserError("this server does not read local paths; send the content as standard input (file \"-\")")
					}
					fh, err := os.Open(config.Expand(f))
					if err != nil {
						return nil, spec.UserError("cannot read %q: %v", f, err)
					}
					defer fh.Close()
					r = fh
				}
				return s.Which(r, ctx.Str("sphere"))
			})
		},
	})
}

func init() {
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "revise", Right: "write",
		Summary:    "Save new content for an artefact as its next version; the earlier versions are kept.",
		Discussion: "The new content is copied into the store. A sealed artefact stays sealed. The metadata embedded in the new content is read again. Holders keep their hold; a tool that extracted text from the earlier version should do it again.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."},
			{Name: "from", Kind: spec.String, Required: true, Help: "The file with the new content."},
		},
		Examples: []string{"artefact revise 01K9B3T0Z6Q8M2C4R7V1X5N9PA --from ~/Desktop/facture-annotée.pdf"},
		Effects:  []string{"Adds a version and appends a revise event."},
		Run: func(ctx *spec.Context) (any, error) {
			if !localPaths(ctx) {
				return nil, spec.UserError("revise reads --from on this machine; this server does not read local paths")
			}
			return run(ctx, func(s *store.Store) (any, error) { return s.Revise(ctx.Str("id"), ctx.Str("from")) })
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "meta", Right: "write",
		Summary:    "Read the metadata embedded in files into their cards, or write the cards into the files as XMP.",
		Discussion: "Needs exiftool. read fills the card's embedded block (date taken or created, title, author, subject, keywords, camera, GPS, pages, size, duration, producer) and, when the card's date is still the deposit date, takes the content's own date. write sets XMP Dublin Core title, date, creator, description and the tags into the file, as a new version (images, PDF and videos only).",
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Required: true, Enum: []string{"read", "write"}, Help: "read or write"},
			{Name: "id", Kind: spec.StringList, Positional: true, Required: true, Help: "Artefact ids or URIs."},
		},
		Examples: []string{"artefact meta read 01K9B3T0Z6Q8M2C4R7V1X5N9PA", "artefact meta write 01K9B3T0Z6Q8M2C4R7V1X5N9PA"},
		Effects:  []string{"read: rewrites the card. write: adds a version with the metadata written into the file."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				var out []any
				for _, id := range ctx.List("id") {
					var r *store.MetaResult
					var err error
					if ctx.Str("verb") == "write" {
						r, err = s.WriteMeta(id)
					} else {
						r, err = s.ReadMeta(id)
					}
					if err != nil {
						if len(ctx.List("id")) == 1 {
							return nil, err
						}
						out = append(out, map[string]any{"id": id, "error": err.Error()})
						continue
					}
					out = append(out, r)
				}
				return out, nil
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "tags", Right: "read",
		Summary:  "List the tags in use, with how many artefacts carry each.",
		Params:   []spec.Param{{Name: "sphere", Kind: spec.String, Help: "This sphere only."}},
		Examples: []string{"artefact tags", "artefact tags --sphere perso"},
		Effects:  []string{"Reads only."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) { return s.TagCounts(ctx.Str("sphere")) })
		},
	})
}
