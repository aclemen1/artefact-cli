package actions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/index"
	"github.com/aclemen1/artefact-cli/internal/journal"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
)

func setup(t *testing.T, journalCmd []string) (string, *config.Config) {
	t.Helper()
	t.Setenv(ByVar, "")
	t.Setenv(actor.OfficeVar, "")
	dir := t.TempDir()
	cfg := &config.Config{Store: filepath.Join(dir, "store"), Spheres: []string{"perso"}}
	cfg.Journal.Command = journalCmd
	path := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(cfg, nil, "test")
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return path, cfg
}

func clock(t *testing.T, day string) {
	old := actor.Now
	t.Cleanup(func() { actor.Now = old })
	d, _ := time.Parse("2006-01-02 15:04", day)
	actor.Now = func() time.Time { return d }
}

func put(t *testing.T, path string, terminal bool, extra ...string) (*spec.Context, any, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(f, []byte("payload "+t.Name()+strings.Join(extra, "")), 0o644)
	a := spec.FindVerb("put")
	argv := append([]string{f, "--sphere", "perso"}, extra...)
	args, err := spec.Parse(a, argv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &spec.Context{Args: args, Config: path, Argv: append([]string{"put"}, argv...), Terminal: terminal, Stdin: strings.NewReader("")}
	res, err := Invoke(a, ctx)
	return ctx, res, err
}

func events(t *testing.T, cfg *config.Config, id string) []store.Event {
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	s, _, err := st.Stat([]string{id}, true)
	if err != nil {
		t.Fatal(err)
	}
	return s[0].Events
}

func TestRequestOrder(t *testing.T) {
	t.Setenv(ByVar, "")
	t.Setenv(actor.OfficeVar, "")
	cases := []struct {
		name string
		ctx  *spec.Context
		tui  bool
		env  map[string]string
		want string
	}{
		{"terminal", &spec.Context{Terminal: true}, false, nil, actor.CLI},
		{"tui", &spec.Context{}, true, nil, actor.TUI},
		{"flag over env", &spec.Context{Args: map[string]any{"by": "agent:x"}, Terminal: true}, false, map[string]string{ByVar: "agent:env"}, "agent:x"},
		{"tool var over office", &spec.Context{}, false, map[string]string{ByVar: "agent:a", actor.OfficeVar: "agent:o"}, "agent:a"},
		{"tui over inherited env", &spec.Context{}, true, map[string]string{ByVar: "agent:a", actor.OfficeVar: "agent:o"}, actor.TUI},
		{"office over terminal", &spec.Context{Terminal: true}, false, map[string]string{actor.OfficeVar: "agent:office-P-0019"}, "agent:office-P-0019"},
		{"header", &spec.Context{Access: &spec.Access{By: "agent:h", Declared: "routine:s"}}, false, nil, "agent:h"},
		{"declared", &spec.Context{Access: &spec.Access{Declared: "routine:s"}}, false, map[string]string{actor.OfficeVar: "agent:o"}, "routine:s"},
		{"no terminal over the server", &spec.Context{Access: &spec.Access{}, Terminal: true}, false, nil, actor.Unknown},
	}
	clock(t, "2026-10-12 10:00")
	for _, c := range cases {
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		got, err := actor.Resolve(Request(c.ctx, c.tui))
		if err != nil || got.By != c.want {
			t.Errorf("%s: %+v %v, want %s", c.name, got, err, c.want)
		}
		for k := range c.env {
			t.Setenv(k, "")
		}
	}
}

func TestPutSigned(t *testing.T) {
	path, cfg := setup(t, nil)
	ctx, res, err := put(t, path, true)
	if err != nil {
		t.Fatal(err)
	}
	ev := events(t, cfg, res.(*store.PutResult).ID)
	if ev[0].By != actor.CLI || len(ctx.Warnings) != 0 {
		t.Errorf("terminal: %+v %v", ev[0], ctx.Warnings)
	}
	_, res, err = put(t, path, false, "--by", "agent:greffier")
	if err != nil {
		t.Fatal(err)
	}
	if ev := events(t, cfg, res.(*store.PutResult).ID); ev[0].By != "agent:greffier" {
		t.Errorf("--by: %+v", ev[0])
	}
	if _, _, err := put(t, path, false, "--by", "contact:AC"); err == nil {
		t.Error("contact:AC accepted as actor")
	}
}

func TestUnsignedThenRefused(t *testing.T) {
	var got []journal.Entry
	var cmds [][]string
	old := JournalStart
	t.Cleanup(func() { JournalStart = old })
	JournalStart = func(c []string, e journal.Entry) error { cmds = append(cmds, c); got = append(got, e); return nil }
	path, cfg := setup(t, []string{"journal", "{id}", "{type}"})

	clock(t, "2026-10-19 21:00")
	ctx, res, err := put(t, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Warnings) != 1 || !strings.Contains(ctx.Warnings[0], actor.Unknown) {
		t.Errorf("warnings %v", ctx.Warnings)
	}
	if ev := events(t, cfg, res.(*store.PutResult).ID); ev[0].By != actor.Unknown {
		t.Errorf("event %+v", ev[0])
	}
	if len(got) != 1 || got[0].Type != "unsigned" || got[0].ID != "cli" || got[0].Sphere != "perso" || got[0].By != actor.Unknown ||
		!strings.Contains(got[0].Text, "outil : artefact") || !strings.Contains(got[0].Text, "commande : artefact put ") {
		t.Errorf("journal %+v", got)
	}

	// hold's --by names the holder, not the actor.
	h := spec.FindVerb("hold")
	args, _ := spec.Parse(h, []string{res.(*store.PutResult).ID, "--by", "mnemo"})
	hctx := &spec.Context{Args: args, Config: path}
	if _, err := Invoke(h, hctx); err != nil || hctx.By != actor.Unknown {
		t.Errorf("hold: %v %q", err, hctx.By)
	}

	clock(t, "2026-10-20 00:00")
	_, _, err = put(t, path, false)
	e, ok := err.(*spec.Error)
	if !ok || e.Code != 2 || e.Kind != "user_error" || !strings.Contains(e.Message, "--by") || !strings.Contains(e.Message, ByVar) {
		t.Fatalf("after the 19th: %#v", err)
	}
	st, _ := store.Open(cfg, nil, "test")
	defer st.Close()
	l, _ := st.List("perso", index.Filter{})
	if len(l) != 1 {
		t.Errorf("a refused put wrote: %d artefacts", len(l))
	}
	// A read needs no actor and gives no warning.
	ls := spec.FindVerb("ls")
	largs, _ := spec.Parse(ls, []string{"--sphere", "perso"})
	lctx := &spec.Context{Args: largs, Config: path}
	if _, err := Invoke(ls, lctx); err != nil || len(lctx.Warnings) != 0 {
		t.Errorf("ls: %v %v", err, lctx.Warnings)
	}
}
