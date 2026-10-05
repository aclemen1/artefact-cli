// Package server serves the actions over REST, the payloads over plain GET
// and MCP over Streamable HTTP, on a Unix socket and on loopback (SPEC §11).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aclemen1/artefact-cli/internal/access"
	"github.com/aclemen1/artefact-cli/internal/actions"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/mcpserver"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

type Server struct {
	CfgPath string
	Cfg     *config.Config
	Tokens  *access.Store
	Version string

	mu    sync.Mutex
	mcps  map[string]*mcp.Server
	views *Views
}

func New(cfgPath string, cfg *config.Config, version string) *Server {
	return &Server{CfgPath: cfgPath, Cfg: cfg, Tokens: access.Open(filepath.Join(cfg.StorePath(), ".artefact")), Version: version, mcps: map[string]*mcp.Server{}}
}

type ctxKey struct{}

// authenticate resolves the caller. On the Unix socket a request without a
// token acts with every sphere and right (as the local CLI would); over
// TCP a token is required.
func (h *Server) authenticate(local bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		secret, hasToken := strings.CutPrefix(auth, "Bearer ")
		if _, pass, ok := r.BasicAuth(); ok && !hasToken {
			// The Finder's WebDAV client sends the token as a password.
			secret, hasToken = pass, true
		}
		var a *spec.Access
		switch {
		case hasToken:
			t, ok := h.Tokens.Verify(secret)
			if !ok {
				writeError(w, spec.Forbidden("invalid or revoked token"))
				return
			}
			var spheres []string
			for _, s := range t.Spheres {
				if slices.Contains(h.Cfg.Spheres, s) {
					spheres = append(spheres, s)
				}
			}
			if len(spheres) == 0 {
				writeError(w, spec.Forbidden("token %q grants no configured sphere", t.Name))
				return
			}
			a = &spec.Access{Spheres: spheres, Rights: t.Rights, Agent: "token:" + t.Name, LocalPaths: local}
		case local:
			agent := r.Header.Get("X-Artefact-Agent")
			if agent == "" {
				agent = "socket"
			}
			a = &spec.Access{Spheres: h.Cfg.Spheres, Rights: access.Rights, Agent: agent, LocalPaths: true}
		default:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			if strings.HasPrefix(r.URL.Path, "/dav/") {
				w.Header().Set("WWW-Authenticate", `Basic realm="artefact"`)
			}
			writeError(w, &spec.Error{Code: 1, Kind: "unauthorized", Message: "a bearer token is required; create one with `artefact token create`"})
			return
		}
		if agent := r.Header.Get("X-Artefact-Agent"); agent != "" && hasToken {
			a.Agent = "token:" + strings.TrimPrefix(a.Agent, "token:") + "/" + agent
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, a)))
	})
}

func accessOf(r *http.Request) *spec.Access {
	a, _ := r.Context().Value(ctxKey{}).(*spec.Access)
	return a
}

// Handler routes REST, payload reads and MCP for one listener.
func (h *Server) Handler(local bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/{action}", h.action)
	mux.HandleFunc("GET /{sphere}/a/{id}", h.payload)
	mux.HandleFunc("GET /{sphere}/a/{id}/{rest}", h.payload)
	mux.HandleFunc("GET /{sphere}/h/sha256/{hex}", h.byDigest)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return h.mcpFor(accessOf(r)) }, nil))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "artefact %s — REST at /v1/<action>, MCP at /mcp\n", h.Version)
	})
	// /dav/ is routed before the mux: its subtree would clash with /{sphere}/….
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/dav/"); ok {
			sphere, _, _ := strings.Cut(rest, "/")
			h.serveDAV(w, r, sphere)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return h.authenticate(local, root)
}

func (h *Server) mcpFor(a *spec.Access) *mcp.Server {
	key := fmt.Sprint(a.Spheres, a.Rights, a.Agent, a.LocalPaths)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.mcps[key]; ok {
		return s
	}
	s := mcpserver.New(h.CfgPath, a, h.Version)
	h.mcps[key] = s
	return s
}

// lazyWriter sets the payload headers on the first write, so that an error
// raised before any byte still goes out as a JSON envelope.
type lazyWriter struct {
	w       http.ResponseWriter
	mime    string
	written bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.written {
		l.written = true
		if l.mime == "" {
			l.mime = "application/octet-stream"
		}
		l.w.Header().Set("Content-Type", l.mime)
	}
	return l.w.Write(p)
}

func (h *Server) run(w http.ResponseWriter, r *http.Request, a *spec.Action, in map[string]any, body io.Reader, mime string) {
	args, err := spec.ArgsFrom(a, in)
	if err != nil {
		writeError(w, err)
		return
	}
	if body == nil {
		body = strings.NewReader("")
	}
	lw := &lazyWriter{w: w, mime: mime}
	res, err := actions.Invoke(a, &spec.Context{Args: args, Config: h.CfgPath, Access: accessOf(r), Stdin: body, Stdout: lw})
	if err != nil {
		if lw.written {
			return // the payload was partly sent; the connection carries the failure
		}
		writeError(w, err)
		return
	}
	if _, ok := res.(spec.Streamed); ok {
		return
	}
	writeJSON(w, http.StatusOK, envelope(res))
}

// action serves POST /v1/<action>. A JSON body carries the parameters; for
// put, a non-JSON body is the payload and the parameters come in the query.
func (h *Server) action(w http.ResponseWriter, r *http.Request) {
	a := spec.FindVerb(r.PathValue("action"))
	if a == nil || a.Right == "" {
		writeError(w, spec.NotFound("no remote action %q; see `artefact schema`", r.PathValue("action")))
		return
	}
	in := map[string]any{}
	var body io.Reader
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, spec.UserError("the body is not a JSON object: %v", err))
			return
		}
	} else {
		if a.Name != "put" && a.Name != "which" {
			writeError(w, spec.UserError("send the parameters of %s as a JSON object (Content-Type: application/json)", a.Name))
			return
		}
		for k, vs := range r.URL.Query() {
			if len(vs) == 1 {
				in[k] = vs[0]
			} else {
				l := make([]any, len(vs))
				for i, v := range vs {
					l[i] = v
				}
				in[k] = l
			}
		}
		in["file"] = "-"
		body = r.Body
	}
	h.run(w, r, a, in, body, "")
}

// payload serves GET /<sphere>/a/<id>[/vN|/card][?reveal=1].
func (h *Server) payload(w http.ResponseWriter, r *http.Request) {
	uri := store.URI(r.PathValue("sphere"), r.PathValue("id"))
	rest := r.PathValue("rest")
	if rest == "card" {
		h.run(w, r, spec.FindVerb("stat"), map[string]any{"id": []any{uri}}, nil, "")
		return
	}
	in := map[string]any{"id": uri}
	if rest != "" {
		in["version"] = rest
	}
	if q := r.URL.Query().Get("reveal"); q == "1" || q == "true" {
		in["reveal"] = true
	}
	h.run(w, r, spec.FindVerb("get"), in, nil, h.mimeOf(r, uri))
}

func (h *Server) byDigest(w http.ResponseWriter, r *http.Request) {
	a := accessOf(r)
	if !a.Can("read") {
		writeError(w, spec.Forbidden("this token lacks the read right"))
		return
	}
	st, err := store.Open(h.Cfg, a.Spheres, a.Agent)
	if err != nil {
		writeError(w, err)
		return
	}
	id, err := st.FindDigest(r.PathValue("sphere"), "sha256:"+strings.ToLower(r.PathValue("hex")))
	st.Close()
	if err != nil {
		var nf *store.NotFound
		if errors.As(err, &nf) {
			err = spec.NotFound("%s", nf.Msg)
		}
		writeError(w, err)
		return
	}
	uri := store.URI(r.PathValue("sphere"), id)
	h.run(w, r, spec.FindVerb("get"), map[string]any{"id": uri}, nil, h.mimeOf(r, uri))
}

func (h *Server) mimeOf(r *http.Request, uri string) string {
	a := accessOf(r)
	st, err := store.Open(h.Cfg, a.Spheres, a.Agent)
	if err != nil {
		return ""
	}
	defer st.Close()
	if ref, err := st.Resolve(uri); err == nil {
		return ref.Row.Mime
	}
	return ""
}

func envelope(res any) map[string]any {
	if res == nil {
		return map[string]any{"ok": true}
	}
	return map[string]any{"ok": true, "result": res}
}

func writeError(w http.ResponseWriter, err error) {
	e := spec.Internal(err)
	status := http.StatusInternalServerError
	switch e.Kind {
	case "user_error":
		status = http.StatusBadRequest
	case "not_found":
		status = http.StatusNotFound
	case "forbidden":
		status = http.StatusForbidden
	case "unauthorized":
		status = http.StatusUnauthorized
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ListenSocket opens the Unix socket with mode 0600, replacing a stale one.
func ListenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("another artefact serve already listens on %s", path)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// ListenLoopback opens addr, which must be a loopback address.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--http takes host:port, e.g. 127.0.0.1:7780: %v", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("--http listens on loopback only (127.0.0.1, ::1 or localhost), got %q", host)
	}
	return net.Listen("tcp", addr)
}

// Serve runs until ctx ends.
func (h *Server) Serve(ctx context.Context, socketPath, httpAddr string) error {
	var servers []*http.Server
	errc := make(chan error, 8)
	views, err := newViews(h.Cfg)
	if err != nil {
		return err
	}
	h.views = views
	defer views.Close()
	go h.syncLoop(ctx)
	go h.renderLoop(ctx)
	nfsListeners, err := h.startNFS(errc)
	if err != nil {
		return err
	}
	defer func() {
		for _, l := range nfsListeners {
			l.Close()
		}
		os.Remove(nfsStatePath(h.Cfg))
	}()
	if socketPath != "" {
		l, err := ListenSocket(socketPath)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: h.Handler(true), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(l) }()
		defer os.Remove(socketPath)
	}
	if httpAddr != "" {
		l, err := ListenLoopback(httpAddr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: h.Handler(false), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(l) }()
	}
	if len(servers) == 0 {
		return fmt.Errorf("nothing to serve: give --socket or --http")
	}
	var mountedDirs []string
	for sp, dirs := range h.Cfg.Views.Mounts {
		for _, dir := range dirs {
			if err := Mount(h.Cfg, sp, dir); err != nil {
				fmt.Fprintf(os.Stderr, "artefact serve: %v\n", err)
				continue
			}
			mountedDirs = append(mountedDirs, dir)
		}
	}
	defer func() {
		for _, d := range mountedDirs {
			if err := Unmount(d); err != nil {
				fmt.Fprintf(os.Stderr, "artefact serve: %v\n", err)
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}

// syncLoop keeps every sphere's backends up to date, at start and then every
// replication.<sphere>.every.
// renderLoop makes the missing text renditions every few minutes, a batch
// at a time, so that new deposits become searchable without a command.
func (h *Server) renderLoop(ctx context.Context) {
	if a := h.Cfg.Lector.Auto; a != nil && !*a {
		return
	}
	st, err := store.Open(h.Cfg, nil, "artefact-render")
	if err != nil {
		fmt.Fprintf(os.Stderr, "artefact serve: render: %v\n", err)
		return
	}
	defer st.Close()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, sp := range h.Cfg.Spheres {
			if _, err := st.RenderMissing(sp, 50, time.Now().Add(4*time.Minute)); err != nil {
				fmt.Fprintf(os.Stderr, "artefact serve: render %s: %v\n", sp, err)
			}
		}
	}
}

func (h *Server) syncLoop(ctx context.Context) {
	for _, sp := range h.Cfg.Spheres {
		pol := h.Cfg.ReplicationOf(sp)
		if len(pol.Backends) == 0 {
			continue
		}
		every, err := time.ParseDuration(pol.Every)
		if err != nil || every <= 0 {
			every = 15 * time.Minute
		}
		go func(sp string, every time.Duration) {
			st, err := store.Open(h.Cfg, []string{sp}, "artefact-sync")
			if err != nil {
				fmt.Fprintf(os.Stderr, "artefact serve: sync %s: %v\n", sp, err)
				return
			}
			defer st.Close()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				res, err := st.Sync(ctx, sp, false)
				if err != nil {
					fmt.Fprintf(os.Stderr, "artefact serve: sync %s: %v\n", sp, err)
				}
				for _, r := range res {
					for _, b := range r.Backends {
						if b.Uploaded > 0 || b.Deleted > 0 || len(b.Errors) > 0 {
							fmt.Fprintf(os.Stderr, "artefact serve: sync %s → %s: %d sent (%d bytes), %d removed, %d error(s)\n", sp, b.Backend, b.Uploaded, b.Bytes, b.Deleted, len(b.Errors))
						}
						for _, e := range b.Errors {
							fmt.Fprintf(os.Stderr, "artefact serve:   %s\n", e)
						}
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}(sp, every)
	}
}
