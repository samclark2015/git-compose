// Package ui provides formatted terminal output helpers.
package ui

import (
	"os"

	"github.com/fatih/color"
)

var (
	colorSection = color.New(color.FgCyan, color.Bold)
	colorOK      = color.New(color.FgGreen)
	colorWarn    = color.New(color.FgYellow)
	colorFail    = color.New(color.FgRed, color.Bold)
	colorInfo    = color.New(color.FgWhite)
)

// Section prints a highlighted section header.
func Section(format string, a ...any) {
	colorSection.Printf("\n=== "+format+" ===\n", a...)
}

// Step prints a step header.
func Step(format string, a ...any) {
	colorInfo.Printf("--- "+format+" ---\n", a...)
}

// OK prints a success message.
func OK(format string, a ...any) {
	colorOK.Printf("  ✓ "+format+"\n", a...)
}

// Warn prints a warning to stderr.
func Warn(format string, a ...any) {
	colorWarn.Fprintf(os.Stderr, "  ⚠ "+format+"\n", a...)
}

// Fail prints a failure message.
func Fail(format string, a ...any) {
	colorFail.Printf("  ✗ "+format+"\n", a...)
}

// Info prints an informational message.
func Info(format string, a ...any) {
	colorInfo.Printf("  "+format+"\n", a...)
}
