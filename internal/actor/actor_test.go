package actor

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func at(s string) func() time.Time {
	loc, _ := time.LoadLocation(Zone)
	t, _ := time.ParseInLocation("2006-01-02 15:04", s, loc)
	return func() time.Time { return t }
}

func TestOrder(t *testing.T) {
	all := map[string]string{"X_BY": "agent:x", OfficeVar: "agent:office-P-0019"}
	cases := []struct {
		name string
		r    Request
		by   string
		from string
	}{
		{"tui first", Request{Flag: "agent:flag", Declared: "routine:d", Var: "X_BY", Getenv: env(all), TUI: true, Terminal: true}, TUI, "tui"},
		{"flag first", Request{Flag: "agent:flag", Declared: "routine:d", Var: "X_BY", Getenv: env(all), Terminal: true}, "agent:flag", "flag"},
		{"declared before env", Request{Declared: "routine:d", Var: "X_BY", Getenv: env(all)}, "routine:d", "config"},
		{"tool var", Request{Var: "X_BY", Getenv: env(all), Terminal: true}, "agent:x", "env:X_BY"},
		{"office", Request{Var: "X_BY", Getenv: env(map[string]string{OfficeVar: "agent:greffier"})}, "agent:greffier", "env:OFFICE_ACTOR"},
		{"tui", Request{Var: "X_BY", Getenv: env(nil), TUI: true, Terminal: true}, TUI, "tui"},
		{"terminal", Request{Var: "X_BY", Getenv: env(nil), Terminal: true}, CLI, "terminal"},
		{"none", Request{Var: "X_BY", Getenv: env(nil), Now: at("2026-10-19 23:59")}, Unknown, "none"},
	}
	for _, c := range cases {
		a, err := Resolve(c.r)
		if err != nil || a.By != c.by || a.From != c.from {
			t.Errorf("%s: got %+v, %v; want %s from %s", c.name, a, err, c.by, c.from)
		}
		if a.Unsigned != (c.by == Unknown) {
			t.Errorf("%s: unsigned %v", c.name, a.Unsigned)
		}
	}
}

func TestDeadline(t *testing.T) {
	r := Request{Var: "X_BY", Getenv: env(nil)}
	r.Now = at("2026-10-19 23:59")
	if a, err := Resolve(r); err != nil || !a.Unsigned {
		t.Fatalf("on the last day: %+v %v", a, err)
	}
	r.Now = at("2026-10-20 00:00")
	_, err := Resolve(r)
	if err == nil || !strings.Contains(err.Error(), "--by") || !strings.Contains(err.Error(), "X_BY") {
		t.Fatalf("after the last day: %v", err)
	}
	// the package clock is the default
	old := Now
	defer func() { Now = old }()
	Now = at("2026-11-01 08:00")
	if _, err := Resolve(Request{Getenv: env(nil)}); err == nil {
		t.Fatal("injected clock ignored")
	}
}

func TestCheck(t *testing.T) {
	for _, bad := range []string{"user", "contact:AC", "agent:", "agent: x", "unknown:cli"} {
		if _, err := Resolve(Request{Flag: bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := Resolve(Request{Getenv: env(map[string]string{OfficeVar: "user"})}); err == nil {
		t.Error("OFFICE_ACTOR=user accepted")
	}
	for _, ok := range []string{"human:AC@tui", "agent:office-P-0019", "routine:brief"} {
		if err := Check(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestEnvAndText(t *testing.T) {
	if got := Env("due-cli", "agent:x"); got[0] != "DUE_CLI_BY=agent:x" || got[1] != "OFFICE_ACTOR=agent:x" {
		t.Errorf("Env: %v", got)
	}
	if Env("task", "") != nil {
		t.Error("Env without actor")
	}
	txt := UnsignedText("alarm", []string{"add", "06:40"})
	if !strings.Contains(txt, "outil : alarm") || !strings.Contains(txt, "commande : alarm add 06:40") {
		t.Errorf("text: %q", txt)
	}
}
