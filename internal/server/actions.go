package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aclemen1/artefact-cli/internal/access"
	"github.com/aclemen1/artefact-cli/internal/actions"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/mcpserver"
	"github.com/aclemen1/artefact-cli/internal/spec"
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
