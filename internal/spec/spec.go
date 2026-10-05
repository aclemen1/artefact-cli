// Package spec holds the single source of truth for every action: the CLI,
// the schema introspection and the MCP server are projections of it.
package spec

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type ParamKind string

const (
	String     ParamKind = "string"
	Bool       ParamKind = "boolean"
	StringList ParamKind = "string[]"
)

type Param struct {
	Name       string    `json:"name"`
	Kind       ParamKind `json:"type"`
	Positional bool      `json:"positional,omitempty"`
	Required   bool      `json:"required,omitempty"`
	Default    string    `json:"default,omitempty"`
	Enum       []string  `json:"enum,omitempty"`
	Help       string    `json:"description"`
	// Hidden aliases absorbed on input, never shown in help or schema.
	Aliases []string `json:"-"`
}

type Action struct {
	Category    string   `json:"category"`
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Discussion  string   `json:"discussion,omitempty"`
	Params      []Param  `json:"params"`
	Examples    []string `json:"examples"`
	Effects     []string `json:"effects"`
	Destructive bool     `json:"destructive"`
	// Meta actions default to text output; action commands default to JSON.
	Meta bool `json:"-"`
	// Run returns the result payload. A nil result yields {"ok": true}.
	Run func(ctx *Context) (any, error) `json:"-"`
	// Text renders the result for --format text. Nil falls back to indented JSON.
	Text func(w io.Writer, result any) `json:"-"`
}

type Context struct {
	Args   map[string]any
	Config string // value of --config, possibly empty
	Format string
	Stdin  io.Reader
	Stdout io.Writer
}

// Streamed is returned by an action that wrote raw bytes to Stdout itself:
// no envelope follows them.
type Streamed struct{}

func (c *Context) Str(name string) string {
	if v, ok := c.Args[name].(string); ok {
		return v
	}
	return ""
}

func (c *Context) Bool(name string) bool {
	v, _ := c.Args[name].(bool)
	return v
}

func (c *Context) List(name string) []string {
	v, _ := c.Args[name].([]string)
	return v
}

// Error is the structured error carried in the envelope.
type Error struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

const (
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitLocked   = 5
)

func UserError(format string, a ...any) *Error {
	return &Error{Code: ExitUsage, Kind: "user_error", Message: fmt.Sprintf(format, a...)}
}

func NotFound(format string, a ...any) *Error {
	return &Error{Code: ExitNotFound, Kind: "not_found", Message: fmt.Sprintf(format, a...)}
}

func Locked(format string, a ...any) *Error {
	return &Error{Code: ExitLocked, Kind: "locked", Message: fmt.Sprintf(format, a...)}
}

func Internal(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: ExitError, Kind: "error", Message: err.Error()}
}

var registry []*Action

func Register(a *Action) { registry = append(registry, a) }

func All() []*Action { return registry }

func Find(category, name string) *Action {
	for _, a := range registry {
		if a.Category == category && a.Name == name {
			return a
		}
	}
	return nil
}

// FindVerb resolves a CLI verb: every action name is unique across categories.
func FindVerb(name string) *Action {
	for _, a := range registry {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func Categories() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range registry {
		if !seen[a.Category] {
			seen[a.Category] = true
			out = append(out, a.Category)
		}
	}
	sort.Strings(out)
	return out
}

// Parse maps argv onto the action's params. Global flags are handled by the caller.
func Parse(a *Action, argv []string) (map[string]any, error) {
	out := map[string]any{}
	byName := map[string]*Param{}
	var positionals []*Param
	for i := range a.Params {
		p := &a.Params[i]
		byName[p.Name] = p
		for _, al := range p.Aliases {
			byName[al] = p
		}
		if p.Positional {
			positionals = append(positionals, p)
		}
	}
	pos := 0
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "--") && len(arg) > 2 {
			name, value, hasValue := strings.Cut(arg[2:], "=")
			p, ok := byName[name]
			if !ok {
				return nil, UserError("unknown option --%s for `artefact %s`. Usage: %s", name, a.Name, usage(a))
			}
			if p.Kind == Bool {
				out[p.Name] = !hasValue || value == "true"
				continue
			}
			if !hasValue {
				if i+1 >= len(argv) {
					return nil, UserError("option --%s needs a value. Example: %s", name, firstExample(a))
				}
				i++
				value = argv[i]
			}
			if err := checkEnum(a, p, value); err != nil {
				return nil, err
			}
			if p.Kind == StringList {
				out[p.Name] = append(asList(out[p.Name]), value)
			} else {
				out[p.Name] = value
			}
			continue
		}
		if pos >= len(positionals) {
			return nil, UserError("unexpected argument %q for `artefact %s`. Usage: %s", arg, a.Name, usage(a))
		}
		p := positionals[pos]
		if err := checkEnum(a, p, arg); err != nil {
			return nil, err
		}
		if p.Kind == StringList {
			out[p.Name] = append(asList(out[p.Name]), arg)
			continue
		}
		out[p.Name] = arg
		pos++
	}
	for _, p := range a.Params {
		if _, set := out[p.Name]; set {
			continue
		}
		if p.Required {
			return nil, UserError("missing %s for `artefact %s`. Example: %s", describeParam(p), a.Name, firstExample(a))
		}
		if p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	return out, nil
}

func asList(v any) []string {
	l, _ := v.([]string)
	return l
}

func checkEnum(a *Action, p *Param, value string) error {
	if len(p.Enum) == 0 {
		return nil
	}
	for _, e := range p.Enum {
		if e == value {
			return nil
		}
	}
	return UserError("%s got %q; expected one of %s. Example: %s", describeParam(*p), value, strings.Join(p.Enum, "|"), firstExample(a))
}

func describeParam(p Param) string {
	if p.Positional {
		return "<" + p.Name + ">"
	}
	return "--" + p.Name
}

func firstExample(a *Action) string {
	if len(a.Examples) > 0 {
		return a.Examples[0]
	}
	return usage(a)
}

func usage(a *Action) string {
	parts := []string{"artefact", a.Name}
	for _, p := range a.Params {
		var s string
		switch {
		case p.Positional:
			s = "<" + p.Name + ">"
		case p.Kind == Bool:
			s = "--" + p.Name
		default:
			s = "--" + p.Name + " <" + strings.ReplaceAll(p.Name, "-", "_") + ">"
		}
		if !p.Required {
			s = "[" + s + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func Usage(a *Action) string { return usage(a) }

// Emit writes the envelope (or text) and returns the process exit code.
func Emit(a *Action, format string, result any, err error) int {
	if format == "" {
		format = "json"
		if a != nil && a.Meta {
			format = "text"
		}
	}
	if err != nil {
		e := Internal(err)
		if format == "text" {
			fmt.Fprintf(os.Stderr, "error: %s\n", e.Message)
		} else {
			writeJSON(os.Stdout, map[string]any{"ok": false, "error": e})
		}
		return e.Code
	}
	if _, ok := result.(Streamed); ok {
		return 0
	}
	code := 0
	if format == "text" {
		switch {
		case a != nil && a.Text != nil && result != nil:
			a.Text(os.Stdout, result)
		case result == nil:
		case isString(result):
			fmt.Fprintln(os.Stdout, result)
		default:
			b, _ := json.MarshalIndent(result, "", "  ")
			fmt.Fprintln(os.Stdout, string(b))
		}
		return code
	}
	if result == nil {
		writeJSON(os.Stdout, map[string]any{"ok": true})
	} else {
		writeJSON(os.Stdout, map[string]any{"ok": true, "result": result})
	}
	return code
}

func isString(v any) bool { _, ok := v.(string); return ok }

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
