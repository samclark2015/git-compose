// Package cli provides a lightweight command dispatch framework.
package cli

import (
	"fmt"
	"os"
	"strings"
)

// Command describes a single sub-command.
type Command struct {
	// Name is the sub-command token as typed on the command line (e.g. "reconcile").
	Name string
	// Usage is the one-line argument synopsis shown in help (e.g. "[--flag] [arg]").
	Usage string
	// Description is the short description shown in the command list.
	Description string
	// Run executes the command. args contains everything after the command name.
	Run func(args []string) error
}

// App is the top-level CLI application.
type App struct {
	// Binary is the program name shown in usage output.
	Binary string
	// EnvVars documents the supported environment variables shown in help.
	EnvVars []EnvVar
	commands []*Command
}

// EnvVar describes an environment variable for the usage output.
type EnvVar struct {
	Name        string
	Description string
	Default     string
}

// Register adds a command to the app.
func (a *App) Register(cmd *Command) {
	a.commands = append(a.commands, cmd)
}

// Run dispatches args to the matching command. args should be os.Args[1:].
// It prints usage and exits with code 1 on errors.
func (a *App) Run(args []string) {
	if len(args) == 0 {
		a.printUsage()
		os.Exit(1)
	}

	name := args[0]
	for _, cmd := range a.commands {
		if cmd.Name == name {
			if err := cmd.Run(args[1:]); err != nil {
				fmt.Fprintf(os.Stderr, "%s failed: %v\n", name, err)
				os.Exit(1)
			}
			return
		}
	}

	fmt.Fprintf(os.Stderr, "unknown command: %s\n", name)
	a.printUsage()
	os.Exit(1)
}

func (a *App) printUsage() {
	b := &strings.Builder{}
	fmt.Fprintf(b, "usage: %s <command> [args]\n\ncommands:\n", a.Binary)

	// Compute column width for alignment.
	width := 0
	for _, cmd := range a.commands {
		if n := len(cmd.Name) + 1 + len(cmd.Usage); n > width {
			width = n
		}
	}

	for _, cmd := range a.commands {
		synopsis := cmd.Name
		if cmd.Usage != "" {
			synopsis += " " + cmd.Usage
		}
		fmt.Fprintf(b, "  %-*s  %s\n", width, synopsis, cmd.Description)
	}

	if len(a.EnvVars) > 0 {
		fmt.Fprintf(b, "\nenvironment variables:\n")
		nameWidth := 0
		for _, e := range a.EnvVars {
			if len(e.Name) > nameWidth {
				nameWidth = len(e.Name)
			}
		}
		for _, e := range a.EnvVars {
			line := fmt.Sprintf("  %-*s  %s", nameWidth, e.Name, e.Description)
			if e.Default != "" {
				line += " (default: " + e.Default + ")"
			}
			fmt.Fprintln(b, line)
		}
	}

	fmt.Fprint(os.Stderr, b.String())
}
