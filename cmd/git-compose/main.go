package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/fatih/color"
)

const (
	defaultRepoDir       = "/opt/homelab"
	defaultCaddyAPI      = "http://127.0.0.1:2019"
	defaultSopsKeyFile   = "/root/.config/sops/age/keys.txt"
	defaultDeployKeyFile = "/root/.ssh/gitops_deploy"
	defaultCaddyNet      = "caddy-net"
	defaultPollAttempts  = "15"
	defaultPollInterval  = "2s"
)

// ---------------------------------------------------------------------------
// output helpers
// ---------------------------------------------------------------------------

var (
	colorSection = color.New(color.FgCyan, color.Bold)
	colorOK      = color.New(color.FgGreen)
	colorWarn    = color.New(color.FgYellow)
	colorFail    = color.New(color.FgRed, color.Bold)
	colorInfo    = color.New(color.FgWhite)
)

func section(format string, a ...any) {
	colorSection.Printf("\n=== "+format+" ===\n", a...)
}

func step(format string, a ...any) {
	colorInfo.Printf("--- "+format+" ---\n", a...)
}

func ok(format string, a ...any) {
	colorOK.Printf("  ✓ "+format+"\n", a...)
}

func warn(format string, a ...any) {
	colorWarn.Fprintf(os.Stderr, "  ⚠ "+format+"\n", a...)
}

func fail(format string, a ...any) {
	colorFail.Printf("  ✗ "+format+"\n", a...)
}

func info(format string, a ...any) {
	colorInfo.Printf("  "+format+"\n", a...)
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "reconcile":
		repoDir := envOr("REPO_DIR", defaultRepoDir)
		routesOnly := false
		changedOnly := false
		for _, arg := range os.Args[2:] {
			if arg == "--routes-only" {
				routesOnly = true
			} else if arg == "--changed-only" {
				changedOnly = true
			} else if !strings.HasPrefix(arg, "-") {
				repoDir = arg
			}
		}
		if err := runReconcile(repoDir, routesOnly, changedOnly); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile failed: %v\n", err)
			os.Exit(1)
		}
	case "register-route":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: git-compose register-route <caddy.json>")
			os.Exit(1)
		}
		caddyAPI := envOr("CADDY_API", defaultCaddyAPI)
		if err := runRegisterRoute(os.Args[2], caddyAPI); err != nil {
			fmt.Fprintf(os.Stderr, "register-route failed: %v\n", err)
			os.Exit(1)
		}
	case "remove-route":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: git-compose remove-route <route-id>")
			os.Exit(1)
		}
		caddyAPI := envOr("CADDY_API", defaultCaddyAPI)
		if err := runRemoveRoute(os.Args[2], caddyAPI); err != nil {
			fmt.Fprintf(os.Stderr, "remove-route failed: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: git-compose <command> [args]

commands:
  reconcile [--routes-only] [--changed-only] [repo-dir]
                                        sync repo and deploy all services (default repo: /opt/homelab)
                                        --routes-only:  skip git sync, services, and image prune; only apply Caddy routes
                                        --changed-only: only deploy services whose files changed since the last reconcile
  register-route <caddy.json>           upsert a single Caddy route from a caddy.json file
  remove-route <route-id>               delete a Caddy route by id

environment variables:
  REPO_DIR          path to the homelab git repo (default: /opt/homelab)
  CADDY_API         Caddy Admin API base URL (default: http://127.0.0.1:2019)
  SOPS_AGE_KEY_FILE path to the SOPS age key file (default: /root/.config/sops/age/keys.txt)
  GIT_DEPLOY_KEY    path to the SSH deploy key (default: /root/.ssh/gitops_deploy)
  CADDY_NET         Docker network name for Caddy (default: caddy-net)
  CADDY_POLL_ATTEMPTS number of attempts to wait for Caddy API (default: 15)
  CADDY_POLL_INTERVAL interval between Caddy poll attempts, e.g. 2s (default: 2s)`)
}
