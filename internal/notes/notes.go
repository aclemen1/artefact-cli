// Package notes reads and adds the notes of an artefact through the commands
// of the configuration (notes.ls, notes.add): artefact names no note tool.
package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aclemen1/artefact-cli/internal/actor"
	"github.com/aclemen1/artefact-cli/internal/config"
)

// Entry is a note, as notes.ls gives it.
type Entry struct {
	ID      string `json:"id"`
	Created string `json:"created"`
	By      string `json:"by,omitempty"`
	Body    string `json:"body"`
}

// Ref is how other tools cite an artefact.
func Ref(id string) string { return "artefact:" + id }

// fill replaces {ref} and {sphere} in argv.
func fill(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = config.Expand(a)
		for k, v := range vars {
			a = strings.ReplaceAll(a, "{"+k+"}", v)
		}
		out[i] = a
	}
	return out
}

func run(argv []string, stdin string, env ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return nil, fmt.Errorf("%s: %v: %s", argv[0], err, msg)
	}
	return out.Bytes(), nil
}

// Configured reports whether notes can be read.
func Configured(cfg *config.Config) bool { return len(cfg.Notes.Ls) > 0 }

// Of reads the notes of the artefact id; nil without notes.ls.
func Of(cfg *config.Config, id, sphere string) ([]Entry, error) {
	if len(cfg.Notes.Ls) == 0 {
		return nil, nil
	}
	out, err := run(fill(cfg.Notes.Ls, map[string]string{"ref": Ref(id), "sphere": sphere}), "")
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	raw := json.RawMessage(out)
	if json.Unmarshal(out, &env) == nil && len(env.Result) > 0 {
		raw = env.Result
	}
	var list []Entry
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrap struct {
			Items []Entry `json:"items"`
			Notes []Entry `json:"notes"`
		}
		if json.Unmarshal(raw, &wrap) != nil {
			return nil, fmt.Errorf("notes.ls: unreadable answer: %.200s", out)
		}
		list = append(wrap.Items, wrap.Notes...)
	}
	return list, nil
}

// Add adds a note to the artefact id, in sphere.
// The actor by fills {by} and goes in the environment of the command:
// <COMMAND>_BY (from the command's name) and OFFICE_ACTOR.
func Add(cfg *config.Config, id, sphere, body, by string) error {
	if len(cfg.Notes.Add) == 0 {
		return fmt.Errorf("no notes.add command in the configuration")
	}
	argv := fill(cfg.Notes.Add, map[string]string{"ref": Ref(id), "sphere": sphere, "by": by})
	_, err := run(argv, body, actor.Env(filepath.Base(argv[0]), by)...)
	return err
}
