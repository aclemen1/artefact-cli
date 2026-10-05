package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aclemen1/artefact-cli/internal/access"
	"github.com/aclemen1/artefact-cli/internal/actions"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/mcpserver"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func load(ctx *spec.Context) (*config.Config, string, error) {
	path := ctx.Config
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", spec.UserError("%v", err)
	}
	return cfg, path, nil
}

func splitSpheres(cfg *config.Config, list string) ([]string, error) {
	if list == "" {
		return cfg.Spheres, nil
	}
	var out []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if !slices.Contains(cfg.Spheres, s) {
			return nil, spec.UserError("sphere %q is not configured; configured spheres: %s", s, strings.Join(cfg.Spheres, ", "))
		}
		out = append(out, s)
	}
	return out, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func init() {
	spec.Register(&spec.Action{
		Category: "ops", Name: "serve",
		Summary:    "Serve the actions over REST and MCP on a Unix socket and, with --http, on a loopback address.",
		Discussion: "On the Unix socket (mode 0600), a request without a token acts with every sphere and right; with a token, within the token's. Over --http, every request needs a token. Routes: POST /v1/<action>, GET /<sphere>/a/<id>[/vN|/card], GET /<sphere>/h/sha256/<hex>, /mcp.",
		Params: []spec.Param{
			{Name: "socket", Kind: spec.String, Help: "Unix socket path. Defaults to serve.socket, then ~/.local/state/artefact/sock."},
			{Name: "http", Kind: spec.String, Help: "Loopback address to also listen on, e.g. 127.0.0.1:7780. Defaults to serve.http."},
		},
		Examples: []string{"artefact serve", "artefact serve --http 127.0.0.1:7780"},
		Effects:  []string{"Listens until interrupted; creates the socket file and removes it on exit."},
		Run: func(ctx *spec.Context) (any, error) {
			cfg, path, err := load(ctx)
			if err != nil {
				return nil, err
			}
			sock := config.Expand(ctx.Str("socket"))
			if sock == "" {
				sock = cfg.SocketPath()
			}
			addr := ctx.Str("http")
			if addr == "" {
				addr = cfg.Serve.HTTP
			}
			c, cancel := signalContext()
			defer cancel()
			fmt.Fprintf(os.Stderr, "artefact %s serving on %s", actions.Version, sock)
			if addr != "" {
				fmt.Fprintf(os.Stderr, " and http://%s", addr)
			}
			fmt.Fprintln(os.Stderr)
			return nil, New(path, cfg, actions.Version).Serve(c, sock, addr)
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "mcp",
		Summary:    "Serve the actions as MCP tools over stdio, for an agent on this machine.",
		Discussion: "The client shares this machine's files: put and get may take local paths. --rights limits what the client may do; reveal and erase are left out unless named.",
		Params: []spec.Param{
			{Name: "spheres", Kind: spec.String, Help: "Comma-separated spheres granted. Defaults to every configured sphere."},
			{Name: "rights", Kind: spec.String, Default: "read,write", Help: "Comma-separated rights granted: read, write, reveal, erase."},
		},
		Examples: []string{"artefact mcp --spheres perso", "claude mcp add artefact -- artefact mcp --spheres perso --rights read,write,reveal"},
		Effects:  []string{"Serves until stdin closes."},
		Run: func(ctx *spec.Context) (any, error) {
			cfg, path, err := load(ctx)
			if err != nil {
				return nil, err
			}
			spheres, err := splitSpheres(cfg, ctx.Str("spheres"))
			if err != nil {
				return nil, err
			}
			rights := strings.Split(ctx.Str("rights"), ",")
			for _, r := range rights {
				if !slices.Contains(access.Rights, r) {
					return nil, spec.UserError("unknown right %q; rights: %s", r, strings.Join(access.Rights, ", "))
				}
			}
			c, cancel := signalContext()
			defer cancel()
			a := &spec.Access{Spheres: spheres, Rights: rights, Agent: "mcp-stdio", LocalPaths: true}
			return spec.Streamed{}, mcpserver.New(path, a, actions.Version).Run(c, &mcp.StdioTransport{})
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "token",
		Summary:    "Create, list or revoke the bearer tokens of remote callers.",
		Discussion: "create shows the token once; only its hash is stored. A token carries spheres and rights (read, write, reveal, erase).",
		Params: []spec.Param{
			{Name: "verb", Kind: spec.String, Positional: true, Required: true, Enum: []string{"create", "list", "revoke"}, Help: "create, list or revoke"},
			{Name: "name", Kind: spec.String, Help: "Name of the client, for create and revoke."},
			{Name: "spheres", Kind: spec.String, Help: "Comma-separated spheres granted, for create. Defaults to the default sphere."},
			{Name: "rights", Kind: spec.String, Default: "read", Help: "Comma-separated rights, for create."},
		},
		Examples: []string{"artefact token create --name mnemo --spheres perso,pro --rights read,write,reveal,erase", "artefact token list", "artefact token revoke --name mnemo"},
		Effects:  []string{"create and revoke rewrite .artefact/tokens.json in the store."},
		Run: func(ctx *spec.Context) (any, error) {
			cfg, _, err := load(ctx)
			if err != nil {
				return nil, err
			}
			ts := New("", cfg, actions.Version).Tokens
			switch ctx.Str("verb") {
			case "list":
				return ts.List()
			case "revoke":
				ok, err := ts.Revoke(ctx.Str("name"))
				if err != nil {
					return nil, err
				}
				if !ok {
					return nil, spec.NotFound("no token named %q; see `artefact token list`", ctx.Str("name"))
				}
				return map[string]any{"revoked": ctx.Str("name")}, nil
			}
			list := ctx.Str("spheres")
			if list == "" {
				list = cfg.DefaultSphere
			}
			spheres, err := splitSpheres(cfg, list)
			if err != nil {
				return nil, err
			}
			secret, pub, err := ts.Create(ctx.Str("name"), spheres, strings.Split(ctx.Str("rights"), ","))
			if err != nil {
				return nil, spec.UserError("%v", err)
			}
			return map[string]any{"token": secret, "name": pub.Name, "spheres": pub.Spheres, "rights": pub.Rights, "note": "shown once; store it in the client's keychain or configuration"}, nil
		},
	})
}

func init() {
	spec.Register(&spec.Action{
		Category: "ops", Name: "mount",
		Summary:    "Mount a sphere's read-only NFS view on a directory; `artefact serve` must be running.",
		Discussion: "The view shows a/<id><ext> (current version) and a/<id>/v<N><ext> for every unsealed artefact; the extension is fixed at deposit. Sealed artefacts never appear. serve also mounts the directories of views.mounts itself, and unmounts them when it stops.",
		Params: []spec.Param{
			{Name: "dir", Kind: spec.String, Positional: true, Required: true, Help: "Directory to mount on; created when absent."},
			{Name: "sphere", Kind: spec.String, Help: "Sphere of the view. Defaults to the default sphere."},
		},
		Examples: []string{"artefact mount ~/mnemo/perso/_artefacts --sphere perso"},
		Effects:  []string{"Mounts the view (read-only, soft) on the directory, as the current user."},
		Run: func(ctx *spec.Context) (any, error) {
			cfg, _, err := load(ctx)
			if err != nil {
				return nil, err
			}
			sp := ctx.Str("sphere")
			if sp == "" {
				sp = cfg.DefaultSphere
			}
			if err := Mount(cfg, sp, ctx.Str("dir")); err != nil {
				return nil, spec.UserError("%v", err)
			}
			return map[string]any{"sphere": sp, "mounted": config.Expand(ctx.Str("dir"))}, nil
		},
	})
	spec.Register(&spec.Action{
		Category: "ops", Name: "unmount",
		Summary:  "Unmount a view mounted by `artefact mount`.",
		Params:   []spec.Param{{Name: "dir", Kind: spec.String, Positional: true, Required: true, Help: "Mounted directory."}},
		Examples: []string{"artefact unmount ~/mnemo/perso/_artefacts"},
		Effects:  []string{"Unmounts the directory; does nothing when no view is mounted there."},
		Run: func(ctx *spec.Context) (any, error) {
			if err := Unmount(ctx.Str("dir")); err != nil {
				return nil, spec.UserError("%v", err)
			}
			return map[string]any{"unmounted": config.Expand(ctx.Str("dir"))}, nil
		},
	})
}

func init() {
	spec.Register(&spec.Action{
		Category: "ops", Name: "locate",
		Summary:    "Give a local file path for an artefact: its place in a mounted view, or else a copy in the user's cache.",
		Discussion: "Used by Artefact.app to open artefact:// links. A sealed artefact gets no path: it is read only with `artefact get --reveal`.",
		Params:     []spec.Param{{Name: "id", Kind: spec.String, Positional: true, Required: true, Help: "Artefact id or URI."}},
		Examples:   []string{"artefact locate artefact://perso/01K9B3T0Z6Q8M2C4R7V1X5N9PA"},
		Effects:    []string{"Without a mounted view, writes a copy (mode 0600) under ~/Library/Caches/artefact/open/<id>/."},
		Run: func(ctx *spec.Context) (any, error) {
			cfg, _, err := load(ctx)
			if err != nil {
				return nil, err
			}
			st, err := store.Open(cfg, nil, "artefact-cli/"+actions.Version)
			if err != nil {
				return nil, err
			}
			defer st.Close()
			ref, err := st.Resolve(ctx.Str("id"))
			if err != nil {
				var nf *store.NotFound
				if errors.As(err, &nf) {
					return nil, spec.NotFound("%s", nf.Msg)
				}
				return nil, spec.UserError("%v", err)
			}
			if ref.Row.Sealed {
				return nil, spec.UserError("artefact %s is sealed; read it with `artefact get %s --reveal --out <file>`", ref.ID, ref.ID)
			}
			if dir, ok := cfg.Views.Mounts[ref.Sphere]; ok && mounted(config.Expand(dir)) {
				p := filepath.Join(config.Expand(dir), "a", ref.ID+ref.Row.Ext)
				if _, err := os.Stat(p); err == nil {
					return map[string]any{"path": p, "from": "view"}, nil
				}
			}
			name := ref.Row.Filename
			if name == "" {
				name = ref.ID + ref.Row.Ext
			}
			home, _ := os.UserHomeDir()
			dir := filepath.Join(home, "Library", "Caches", "artefact", "open", ref.ID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			p := filepath.Join(dir, filepath.Base(name))
			rc, _, err := st.OpenPayload(ref.ID, "", false)
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(f, rc); err != nil {
				f.Close()
				return nil, err
			}
			return map[string]any{"path": p, "from": "cache"}, f.Close()
		},
	})
}
