package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aclemen1/artefact-cli/client"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/store"
)

type env struct {
	srv    *Server
	sock   string
	base   string
	cancel context.CancelFunc
}

func start(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Store: filepath.Join(dir, "store"), Spheres: []string{"perso", "pro"}, DefaultSphere: "perso"}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(cfg, nil, "test")
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Unix socket paths are limited to 104 bytes on macOS.
	sockDir, err := os.MkdirTemp("/tmp", "art")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg, _ = config.Load(cfgPath)
	srv := New(cfgPath, cfg, "test")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, sock, addr) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &env{srv: srv, sock: sock, base: "http://" + addr, cancel: cancel}
}

func (e *env) token(t *testing.T, name string, spheres, rights []string) string {
	t.Helper()
	secret, _, err := e.srv.Tokens.Create(name, spheres, rights)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func kind(err error) string {
	if e, ok := err.(*client.Error); ok {
		return e.Kind
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

func TestSocketWithoutToken(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	c := client.NewSocket(e.sock, client.WithAgent("mnemo/test"))
	r, err := c.Put(ctx, strings.NewReader("hello socket"), client.PutOptions{Filename: "h.txt", Holds: []client.Holder{{By: "mnemo", Ref: "C1"}}, Key: "C1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "created" || r.Sphere != "perso" {
		t.Fatalf("put %+v", r)
	}
	again, err := c.Put(ctx, strings.NewReader("hello socket"), client.PutOptions{Holds: []client.Holder{{By: "mnemo", Ref: "C2"}}})
	if err != nil || again.Status != "known" || len(again.Holders) != 2 {
		t.Fatalf("known %+v %v", again, err)
	}
	rc, err := c.Open(ctx, r.URI, "", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello socket" {
		t.Errorf("payload %q", b)
	}
	out := filepath.Join(t.TempDir(), "o.txt")
	if err := c.GetFile(ctx, r.URI, out, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	stats, missing, err := c.Stat(ctx, r.ID, "01K9B3T0Z6Q8M2C4R7V1X5N9PA")
	if err != nil || len(stats) != 1 || len(missing) != 1 {
		t.Fatalf("stat %v %v %v", stats, missing, err)
	}
	held, err := c.List(ctx, client.ListOptions{HeldBy: "mnemo:C2"})
	if err != nil || len(held) != 1 {
		t.Fatalf("held %v %v", held, err)
	}
	if n, err := c.Release(ctx, r.ID, "mnemo", ""); err != nil || n != 2 {
		t.Fatalf("release %d %v", n, err)
	}
	plan, err := c.PlanErase(ctx, []string{r.ID}, "")
	if err != nil || plan.Code == "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	proof, err := c.ConfirmErase(ctx, plan.Code)
	if err != nil || len(proof.Erased) != 1 || !proof.Locations[0].Verified {
		t.Fatalf("proof %+v %v", proof, err)
	}
	if _, err := c.Open(ctx, r.URI, "", false); !client.IsNotFound(err) {
		t.Errorf("erased artefact: %v", err)
	}
	// --force is for a human at the CLI.
	r2, _ := c.Put(ctx, strings.NewReader("held"), client.PutOptions{Holds: []client.Holder{{By: "x"}}})
	err = c.Call(ctx, "erase", map[string]any{"id": []string{r2.ID}, "force": true, "plan": true}, nil)
	if kind(err) != "forbidden" {
		t.Errorf("force over the socket: %v", err)
	}
	// Local CLI actions stay local.
	if err := c.Call(ctx, "token", map[string]any{"verb": "list"}, nil); kind(err) != "not_found" {
		t.Errorf("token over the server: %v", err)
	}
}

func TestTokensOverHTTP(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	if _, err := client.NewHTTP(e.base).List(ctx, client.ListOptions{}); kind(err) != "unauthorized" {
		t.Fatalf("no token over TCP: %v", err)
	}
	if _, err := client.NewHTTP(e.base, client.WithToken("art_wrong")).List(ctx, client.ListOptions{}); kind(err) != "forbidden" {
		t.Fatalf("bad token: %v", err)
	}
	writer := client.NewHTTP(e.base, client.WithToken(e.token(t, "w", []string{"perso", "pro"}, []string{"read", "write"})))
	pro, err := writer.Put(ctx, strings.NewReader("pro doc"), client.PutOptions{Sphere: "pro", Filename: "p.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	reader := client.NewHTTP(e.base, client.WithToken(e.token(t, "r", []string{"perso"}, []string{"read"})))
	if _, err := reader.Put(ctx, strings.NewReader("x"), client.PutOptions{}); kind(err) != "forbidden" {
		t.Errorf("put with a read token: %v", err)
	}
	if _, err := reader.Open(ctx, pro.URI, "", false); kind(err) != "user_error" {
		t.Errorf("pro artefact through a perso token: %v", err)
	}
	if _, missing, _ := reader.Stat(ctx, pro.ID); len(missing) != 1 {
		t.Errorf("a pro id must be missing for a perso token")
	}
	if err := reader.Call(ctx, "get", map[string]any{"id": pro.ID, "reveal": true}, nil); kind(err) != "forbidden" {
		t.Errorf("reveal without the right: %v", err)
	}
	if err := writer.Call(ctx, "get", map[string]any{"id": pro.ID, "out": "/tmp/x"}, nil); kind(err) != "user_error" {
		t.Errorf("--out over TCP: %v", err)
	}
	if err := writer.Call(ctx, "erase", map[string]any{"id": []string{pro.ID}, "plan": true}, nil); kind(err) != "forbidden" {
		t.Errorf("erase without the right: %v", err)
	}
	// Content addressing and card routes.
	req, _ := http.NewRequest("GET", e.base+"/pro/h/sha256/"+strings.TrimPrefix(pro.Digest, "sha256:"), nil)
	req.Header.Set("Authorization", "Bearer "+e.token(t, "r2", []string{"pro"}, []string{"read"}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "pro doc" || resp.Header.Get("Content-Type") != "application/pdf" {
		t.Errorf("by digest: %q %s", b, resp.Header.Get("Content-Type"))
	}
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPOverHTTP(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	tok := e.token(t, "agent", []string{"perso"}, []string{"read", "write"})
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := cl.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["put"] || !names["get"] || names["erase"] || names["serve"] {
		t.Errorf("tools %v", names)
	}
	callJSON := func(name string, args map[string]any) map[string]any {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]any
		_ = json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &env)
		return env
	}
	put := callJSON("put", map[string]any{"content_base64": base64.StdEncoding.EncodeToString([]byte("via mcp")), "filename": "m.txt"})
	if put["ok"] != true {
		t.Fatalf("put %v", put)
	}
	id := put["result"].(map[string]any)["id"].(string)
	got := callJSON("get", map[string]any{"id": id})
	raw, _ := base64.StdEncoding.DecodeString(got["result"].(map[string]any)["content_base64"].(string))
	if string(raw) != "via mcp" {
		t.Errorf("get %v", got)
	}
	if env := callJSON("put", map[string]any{"file": "/etc/hosts"}); env["ok"] != false {
		t.Errorf("a local path over HTTP must be refused: %v", env)
	}
	rr, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: "artefact://perso/" + id})
	if err != nil || rr.Contents[0].Text != "via mcp" {
		t.Errorf("resource %v %v", rr, err)
	}
}
