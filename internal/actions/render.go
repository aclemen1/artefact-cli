package actions

import (
	"fmt"
	"strconv"

	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func init() {
	spec.Register(&spec.Action{
		Category: "artefacts", Name: "render", Right: "write",
		Summary:    "Make the text rendition of artefacts with lector.",
		Discussion: "The rendition is the Markdown text of the current version; search and deciders read it. Sealed artefacts are never sent to lector. A rendition already made for the current version is kept unless --force.",
		Params: []spec.Param{
			{Name: "id", Kind: spec.StringList, Positional: true, Help: "Artefact ids or URIs. Without ids, --missing is required."},
			{Name: "missing", Kind: spec.Bool, Help: "Render every unsealed artefact of the granted spheres that has no current rendition."},
			{Name: "sphere", Kind: spec.String, Help: "With --missing, this sphere only."},
			{Name: "limit", Kind: spec.String, Default: "0", Help: "With --missing, at most this many artefacts; 0 for all."},
			{Name: "force", Kind: spec.Bool, Help: "Render again even when the current version has a rendition."},
		},
		Examples: []string{"artefact render 01K9B3T0Z6Q8M2C4R7V1X5N9PA", "artefact render --missing --sphere perso --limit 200"},
		Effects:  []string{"Runs lector on the clear payload; writes the text under the object's extensions and updates the card and the search index."},
		Run: func(ctx *spec.Context) (any, error) {
			ids := ctx.List("id")
			if len(ids) == 0 && !ctx.Bool("missing") {
				return nil, spec.UserError("name artefacts, or pass --missing. Example: artefact render --missing --sphere perso")
			}
			limit, err := strconv.Atoi(ctx.Str("limit"))
			if err != nil {
				return nil, spec.UserError("--limit takes a number, got %q", ctx.Str("limit"))
			}
			return run(ctx, func(s *store.Store) (any, error) {
				if ctx.Bool("missing") {
					spheres := s.Spheres
					if sp := ctx.Str("sphere"); sp != "" {
						spheres = []string{sp}
					}
					for _, sp := range spheres {
						more, err := s.Unrendered(sp)
						if err != nil {
							return nil, err
						}
						ids = append(ids, more...)
					}
					if limit > 0 && len(ids) > limit {
						ids = ids[:limit]
					}
				}
				counts := map[string]int{}
				var results []*store.RenderResult
				errs := map[string]string{}
				for _, id := range ids {
					r, err := s.Render(id, ctx.Bool("force"))
					if err != nil {
						counts["error"]++
						errs[id] = err.Error()
						continue
					}
					counts[r.Status]++
					results = append(results, r)
				}
				if len(ids) == 1 && len(errs) == 1 {
					return nil, fmt.Errorf("%s", errs[ids[0]])
				}
				return map[string]any{"counts": counts, "artefacts": results, "errors": errs}, nil
			})
		},
	})
}
