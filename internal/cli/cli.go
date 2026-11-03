// Package cli is the ply command line: argument parsing and output. Every
// command takes --json, and none prompt when given their arguments.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/blatessa/ply/internal/plyerr"
)

// Args are a command's parsed arguments.
type Args struct {
	Pos   []string
	Flags map[string]string
	JSON  bool
}

func (a *Args) Bool(name string) bool  { _, ok := a.Flags[name]; return ok }
func (a *Args) Str(name string) string { return a.Flags[name] }

// Arg returns positional argument i, or "".
func (a *Args) Arg(i int) string {
	if i < len(a.Pos) {
		return a.Pos[i]
	}
	return ""
}

type command struct {
	name    string
	usage   string
	summary string
	// valued lists flags that take a value; other flags are booleans.
	valued []string
	bools  []string
	// optional lists flags whose value may be left out: a bare --on means
	// "." (the folder's task). The next argument is taken as the value only
	// if it looks like one (see looksLikeValue).
	optional []string
	minPos   int
	maxPos   int // -1: any
	run      func(ctx context.Context, c *CLI, a *Args) (any, error)
}

// CLI runs commands, writing to Out and Err.
type CLI struct {
	Out, Err io.Writer
	Dir      string
	json     bool
}

var commands []*command

func register(c *command) { commands = append(commands, c) }

func find(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

func parse(c *command, args []string) (*Args, error) {
	a := &Args{Flags: map[string]string{}}
	valued := map[string]bool{"project": true}
	for _, v := range c.valued {
		valued[v] = true
	}
	optional := map[string]bool{}
	for _, o := range c.optional {
		optional[o] = true
	}
	bools := map[string]bool{"json": true, "help": true}
	for _, b := range c.bools {
		bools[b] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			a.Pos = append(a.Pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			a.Pos = append(a.Pos, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case name == "h":
			name = "help"
		case name == "m" && c.name == "label":
			name = "message"
		}
		switch {
		case optional[name]:
			if !hasVal {
				val = "."
				if i+1 < len(args) && looksLikeValue(args[i+1]) {
					i++
					val = args[i]
				}
			}
			a.Flags[name] = val
		case valued[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, plyerr.New(plyerr.Invalid, "ply help "+c.name, "--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			a.Flags[name] = val
		case bools[name]:
			if hasVal {
				return nil, plyerr.New(plyerr.Invalid, "ply help "+c.name, "--%s doesn't take a value", name)
			}
			a.Flags[name] = ""
		default:
			return nil, plyerr.New(plyerr.Invalid, "ply help "+c.name, "unknown flag --%s for `ply %s`", name, c.name)
		}
	}
	a.JSON = a.Bool("json")
	return a, nil
}

// Run runs ply with the given arguments (without the program name) and
// returns the exit code.
func (c *CLI) Run(ctx context.Context, argv []string) int {
	if len(argv) == 0 {
		c.help(nil)
		return 0
	}
	name, rest := argv[0], argv[1:]
	// Two-word commands: "server start", "server new".
	if (name == "server") && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = name+" "+rest[0], rest[1:]
	}
	switch name {
	case "help", "--help", "-h":
		c.help(rest)
		return 0
	case "version", "--version":
		fmt.Fprintln(c.Out, "ply", Version)
		return 0
	}
	cmd := find(name)
	if cmd == nil {
		c.json = contains(rest, "--json")
		return c.fail(plyerr.New(plyerr.Invalid, "ply help", "unknown command %q", name))
	}
	a, err := parse(cmd, rest)
	if err != nil {
		c.json = contains(rest, "--json")
		return c.fail(err)
	}
	c.json = a.JSON
	if a.Bool("help") {
		c.help([]string{cmd.name})
		return 0
	}
	if len(a.Pos) < cmd.minPos || (cmd.maxPos >= 0 && len(a.Pos) > cmd.maxPos) {
		return c.fail(plyerr.New(plyerr.Invalid, "ply help "+cmd.name, "usage: ply %s", cmd.usage))
	}
	out, err := cmd.run(ctx, c, a)
	if err != nil {
		return c.fail(err)
	}
	if c.json {
		if out == nil {
			out = map[string]bool{"ok": true}
		}
		enc := json.NewEncoder(c.Out)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	}
	return 0
}

// looksLikeValue reports whether s is a task reference or "project", so a
// title after a bare --on isn't mistaken for its value.
func looksLikeValue(s string) bool {
	if s == "project" || s == "." {
		return true
	}
	s = strings.TrimPrefix(s, "#")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (c *CLI) fail(err error) int {
	if c.json {
		e, ok := plyerr.As(err)
		if !ok {
			e = &plyerr.Error{Code: plyerr.Internal, Msg: err.Error()}
		}
		enc := json.NewEncoder(c.Out)
		enc.SetIndent("", "  ")
		enc.Encode(e)
		return 1
	}
	fmt.Fprintf(c.Err, "ply: %v\n", err)
	if e, ok := plyerr.As(err); ok && len(e.Paths) > 0 {
		for _, p := range e.Paths {
			fmt.Fprintf(c.Err, "  %s\n", p)
		}
	}
	return 1
}

// say prints human output (suppressed with --json).
func (c *CLI) say(format string, args ...any) {
	if !c.json {
		fmt.Fprintf(c.Out, format+"\n", args...)
	}
}

func (c *CLI) help(args []string) {
	if len(args) > 0 {
		if cmd := find(strings.Join(args, " ")); cmd != nil {
			fmt.Fprintf(c.Out, "usage: ply %s\n\n%s\n", cmd.usage, cmd.summary)
			return
		}
	}
	fmt.Fprintln(c.Out, "Ply: source control built on tasks.\n\nusage: ply <command> [arguments] [--json]\n\nCommands:")
	for _, cmd := range commands {
		fmt.Fprintf(c.Out, "  %-22s %s\n", cmd.name, firstLine(cmd.summary))
	}
	fmt.Fprintln(c.Out, "\nRun `ply help <command>` for details.")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// Version is set at build time.
var Version = "dev"

// Main is the program entry point.
func Main() {
	dir, _ := os.Getwd()
	c := &CLI{Out: os.Stdout, Err: os.Stderr, Dir: dir}
	os.Exit(c.Run(context.Background(), os.Args[1:]))
}
