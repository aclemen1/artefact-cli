package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/aclemen1/artefact-cli/internal/actions"
	_ "github.com/aclemen1/artefact-cli/internal/server"
	"github.com/aclemen1/artefact-cli/internal/spec"
)

const rootHelp = `artefact — files kept with a stable id, a card, versions and copies.

Usage: artefact <action> [arguments] [--config <path>] [--format json|text]

MESSAGE FOR LLM / AI AGENTS: run ` + "`artefact schema`" + ` to list categories, then
` + "`artefact schema <category> <action>`" + ` for the exact parameters, examples and
effects of one action. Copy an example from there.

Actions by category:
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	var configFlag, format string
	help := false
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--config" && i+1 < len(argv):
			configFlag = argv[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			configFlag = strings.TrimPrefix(a, "--config=")
		case a == "--format" && i+1 < len(argv):
			format = argv[i+1]
			i++
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "--json":
			format = "json"
		case a == "--help" || a == "-h":
			help = true
		case a == "--version":
			rest = append(rest, "version")
		default:
			rest = append(rest, a)
		}
	}
	if format != "" && format != "json" && format != "text" {
		return spec.Emit(nil, "json", nil, spec.UserError("--format takes json or text, got %q. Example: artefact ls --format text", format))
	}
	if len(rest) == 0 {
		printRoot()
		return 0
	}
	act := spec.FindVerb(rest[0])
	if act == nil {
		return spec.Emit(nil, format, nil, spec.UserError("unknown action %q. Run `artefact schema` to list actions, for example `artefact put`", rest[0]))
	}
	if help {
		spec.TextSchema(os.Stdout, spec.Leaf{Action: act, Usage: spec.Usage(act)})
		return 0
	}
	args, err := spec.Parse(act, rest[1:])
	if err != nil {
		return spec.Emit(act, format, nil, err)
	}
	result, err := act.Run(&spec.Context{Args: args, Config: configFlag, Format: format, Stdin: os.Stdin, Stdout: os.Stdout})
	return spec.Emit(act, format, result, err)
}

func printRoot() {
	fmt.Print(rootHelp)
	for _, c := range spec.Categories() {
		var names []string
		for _, a := range spec.ActionsIn(c) {
			names = append(names, a.Name)
		}
		fmt.Printf("  %-10s %s\n", c, strings.Join(names, ", "))
	}
	fmt.Printf("\nVersion %s\n", actions.Version)
}
