package actions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/backend"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func init() {
	spec.Register(&spec.Action{
		Category: "ops", Name: "sync", Right: "write",
		Summary:    "Copy to each backend of the sphere what it lacks, and remove from it what the store no longer holds.",
		Discussion: "Each backend ends up holding the sphere's OCFL storage root, file for file; the card and event log of a sealed artefact travel encrypted with age. serve runs a sync every replication.<sphere>.every (15m by default). seal, redact --purge and erase already bring their object up to date at once.",
		Params: []spec.Param{
			{Name: "sphere", Kind: spec.String, Help: "Sync this sphere only."},
			{Name: "dry-run", Kind: spec.Bool, Help: "Count what would be sent and removed, change nothing."},
		},
		Examples: []string{"artefact sync", "artefact sync --sphere perso --dry-run"},
		Effects:  []string{"Uploads to and deletes from the backends; records each copied file in the index."},
		Run: func(ctx *spec.Context) (any, error) {
			return run(ctx, func(s *store.Store) (any, error) {
				return s.Sync(context.Background(), ctx.Str("sphere"), ctx.Bool("dry-run"))
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "fsck", Right: "write",
		Summary:    "Verify the digests of the local objects and of their copies on the backends.",
		Discussion: "A copy that is missing, of the wrong size or of the wrong SHA-256 is dropped from the record; the next sync sends it again. Without --deep, a backend that gives no SHA-256 is checked by size only.",
		Params: []spec.Param{
			{Name: "sphere", Kind: spec.String, Help: "Check this sphere only."},
			{Name: "sample", Kind: spec.String, Default: "0", Help: "Check this many artefacts and copies at random; 0 checks all."},
			{Name: "deep", Kind: spec.Bool, Help: "Download a copy whose backend gives no SHA-256, to hash it."},
		},
		Examples: []string{"artefact fsck", "artefact fsck --sphere perso --sample 50"},
		Effects:  []string{"Reads the store and the backends; updates the verification dates."},
		Run: func(ctx *spec.Context) (any, error) {
			n, err := strconv.Atoi(ctx.Str("sample"))
			if err != nil || n < 0 {
				return nil, spec.UserError("--sample takes a number, got %q. Example: artefact fsck --sample 50", ctx.Str("sample"))
			}
			return run(ctx, func(s *store.Store) (any, error) {
				return s.Fsck(context.Background(), ctx.Str("sphere"), n, ctx.Bool("deep"))
			})
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "backend",
		Summary:    "Add a backend to a sphere's replication, or list the backends.",
		Discussion: "add --type drive runs Google Drive's consent in the browser (scope drive.file: the remote sees only the files artefact creates; deletions skip the Drive trash), keeps the token in ~/.config/artefact/rclone.conf, then adds the backend to replication.<sphere>. add --remote uses an rclone remote path as it is. A backend belongs to one sphere.",
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Required: true, Enum: []string{"add", "ls", "reconnect"}, Help: "add, ls, or reconnect (consent again, with your own OAuth client)"},
			{Name: "name", Kind: spec.String, Positional: true, Help: "Backend name, for add, e.g. gdrive-perso."},
			{Name: "sphere", Kind: spec.String, Help: "Sphere of the backend, for add."},
			{Name: "type", Kind: spec.String, Enum: []string{"drive", "remote"}, Default: "remote", Help: "drive: create a Google Drive remote; remote: use --remote as given."},
			{Name: "remote", Kind: spec.String, Help: "rclone remote path. Defaults to <name>:artefact for --type drive."},
			{Name: "untrusted", Kind: spec.Bool, Help: "Its copies do not count towards numcopies and mincopies."},
			{Name: "client-id", Kind: spec.String, Help: "Your Google OAuth client id, for add --type drive or reconnect. reconnect without it keeps the configured client."},
			{Name: "client-secret", Kind: spec.String, Help: "Its secret, for add --type drive or reconnect."},
		},
		Examples: []string{"artefact backend add gdrive-perso --sphere perso --type drive", "artefact backend add disk --sphere perso --remote :local:/Volumes/Backup/artefact", "artefact backend ls", "artefact backend reconnect gdrive-perso --client-id 123.apps.googleusercontent.com --client-secret GOCSPX-…"},
		Effects:  []string{"add: may open the browser for consent; writes rclone.conf and the artefact configuration.", "reconnect: opens the browser for consent; forgets the backend's copy records, so the next sync sends every file again."},
		Run: func(ctx *spec.Context) (any, error) {
			path := ctx.Config
			if path == "" {
				path = config.DefaultPath()
			}
			cfg, err := config.Load(path)
			if err != nil {
				return nil, spec.UserError("%v", err)
			}
			if ctx.Str("verb") == "ls" {
				s, err := openAs(ctx, cfg, nil)
				if err != nil {
					return nil, wrap(err)
				}
				defer s.Close()
				cov, err := s.Coverage("")
				if err != nil {
					return nil, err
				}
				return map[string]any{"backends": cfg.Backends, "replication": cfg.Replication, "coverage": cov}, nil
			}
			if ctx.Str("verb") == "reconnect" {
				name := ctx.Str("name")
				b, ok := cfg.Backends[name]
				if !ok {
					return nil, spec.UserError("no backend %q; see `artefact backend ls`", name)
				}
				if (ctx.Str("client-id") == "") != (ctx.Str("client-secret") == "") {
					return nil, spec.UserError("give --client-id and --client-secret together")
				}
				rc := cfg.RcloneConfig
				if rc == "" {
					rc = backend.ConfigPath()
				}
				backend.Init(rc)
				fmt.Fprintf(os.Stderr, "Google Drive consent for %q: your browser opens; sign in with the account of sphere %q.\n", name, b.Sphere)
				if err := backend.CreateDriveWith(context.Background(), name, ctx.Str("client-id"), ctx.Str("client-secret")); err != nil {
					return nil, fmt.Errorf("drive set-up: %w", err)
				}
				_ = os.Chmod(rc, 0o600)
				account, err := backend.DriveAccount(context.Background(), name)
				if err != nil {
					backend.DropToken(name)
					return nil, fmt.Errorf("cannot tell which account was authorized, so the token is dropped: %w", err)
				}
				if b.Account != "" && !strings.EqualFold(account, b.Account) {
					backend.DropToken(name)
					return nil, spec.UserError("the consent was given as %s, but %s must use %s; the token is dropped and nothing is copied. Run it again and pick %s (a private browser window avoids the account Google picks by itself): artefact backend reconnect %s", account, name, b.Account, b.Account, name)
				}
				// A new client or account sees none of the earlier copies (scope drive.file):
				// forget them so that the next sync sends everything again.
				s, err := openAs(ctx, cfg, []string{b.Sphere})
				if err != nil {
					return nil, wrap(err)
				}
				defer s.Close()
				forgotten, err := s.ForgetBackend(b.Sphere, name)
				if err != nil {
					return nil, err
				}
				clientID, _ := backend.ClientID(name)
				return map[string]any{"backend": name, "account": account, "client_id": clientID, "copies_forgotten": forgotten,
					"next": "restart serve: launchctl kickstart -k gui/$(id -u)/aero.clement.artefact"}, nil
			}
			name, sp := ctx.Str("name"), ctx.Str("sphere")
			if name == "" || !slices.Contains(cfg.Spheres, sp) {
				return nil, spec.UserError("backend add takes a name and a configured sphere. Example: artefact backend add gdrive-perso --sphere perso --type drive")
			}
			if _, exists := cfg.Backends[name]; exists {
				return nil, spec.UserError("backend %q exists already", name)
			}
			remote := ctx.Str("remote")
			if ctx.Str("type") == "drive" {
				rc := cfg.RcloneConfig
				if rc == "" {
					rc = backend.ConfigPath()
				}
				if err := os.MkdirAll(filepath.Dir(rc), 0o700); err != nil {
					return nil, err
				}
				backend.Init(rc)
				fmt.Fprintf(os.Stderr, "Google Drive consent for %q: your browser opens; sign in with the account of sphere %q.\n", name, sp)
				if err := backend.CreateDriveWith(context.Background(), name, ctx.Str("client-id"), ctx.Str("client-secret")); err != nil {
					return nil, fmt.Errorf("drive set-up: %w", err)
				}
				_ = os.Chmod(rc, 0o600)
				if remote == "" {
					remote = name + ":artefact"
				}
			}
			if remote == "" {
				return nil, spec.UserError("--remote names the rclone remote path. Example: --remote :local:/Volumes/Backup/artefact")
			}
			if cfg.Backends == nil {
				cfg.Backends = map[string]config.Backend{}
			}
			b := config.Backend{Remote: remote, Sphere: sp}
			if ctx.Bool("untrusted") {
				f := false
				b.Trusted = &f
			}
			cfg.Backends[name] = b
			if cfg.Replication == nil {
				cfg.Replication = map[string]config.Replication{}
			}
			r := cfg.Replication[sp]
			r.Backends = append(r.Backends, name)
			if r.NumCopies == 0 {
				r.NumCopies = 2
			}
			if r.MinCopies == 0 {
				r.MinCopies = 1
			}
			cfg.Replication[sp] = r
			if err := cfg.Validate(); err != nil {
				return nil, spec.UserError("%v", err)
			}
			if err := cfg.Save(path); err != nil {
				return nil, err
			}
			return map[string]any{"backend": name, "remote": remote, "sphere": sp, "replication": r,
				"next": "run `artefact sync --sphere " + sp + "`, or wait for serve's next sync (restart serve to load the new configuration)"}, nil
		},
	})
}
