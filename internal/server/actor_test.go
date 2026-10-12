package server

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aclemen1/artefact-cli/client"
	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/store"
)

// The other tests write as an agent: the clients send X-Artefact-By.
func TestMain(m *testing.M) {
	os.Setenv("ARTEFACT_BY", "agent:test")
	os.Exit(m.Run())
}

func lastBy(t *testing.T, e *env, id string) string {
	t.Helper()
	st, err := store.Open(e.srv.Cfg, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _, err := st.Stat([]string{id}, true)
	if err != nil || len(s) != 1 || len(s[0].Events) == 0 {
		t.Fatalf("stat %v %v", s, err)
	}
	return s[0].Events[len(s[0].Events)-1].By
}

func TestActorOfARequest(t *testing.T) {
	t.Setenv("ARTEFACT_BY", "")
	t.Setenv("OFFICE_ACTOR", "")
	e := start(t)
	ctx := context.Background()

	// The header of the client comes first.
	c := client.NewSocket(e.sock, client.WithAgent("mnemo"), client.WithBy("agent:office-P-0019"))
	r, err := c.Put(ctx, strings.NewReader("by header"), client.PutOptions{Sphere: "perso", Filename: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lastBy(t, e, r.ID); got != "agent:office-P-0019" {
		t.Errorf("header: %q", got)
	}

	// Without header, the actor declared for the server.
	e.srv.Cfg.Serve.By = "routine:artefact-serve"
	bare := client.NewSocket(e.sock)
	r, err = bare.Put(ctx, strings.NewReader("by config"), client.PutOptions{Sphere: "perso", Filename: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lastBy(t, e, r.ID); got != "routine:artefact-serve" {
		t.Errorf("declared: %q", got)
	}

	// Without either: unknown:cli through the 19th, refused after.
	e.srv.Cfg.Serve.By = ""
	old := actor.Now
	defer func() { actor.Now = old }()
	actor.Now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }
	r, err = bare.Put(ctx, strings.NewReader("unsigned"), client.PutOptions{Sphere: "perso", Filename: "c.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lastBy(t, e, r.ID); got != actor.Unknown {
		t.Errorf("unsigned: %q", got)
	}
	actor.Now = func() time.Time { return time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC) }
	_, err = bare.Put(ctx, strings.NewReader("refused"), client.PutOptions{Sphere: "perso", Filename: "d.txt"})
	if kind(err) != "user_error" || !strings.Contains(err.Error(), "ARTEFACT_BY") {
		t.Fatalf("after the 19th: %v", err)
	}
	// A read needs no actor.
	if _, _, err := bare.Stat(ctx, r.ID); err != nil {
		t.Errorf("read refused: %v", err)
	}
}
