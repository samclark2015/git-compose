package main

import (
	"fmt"
	"os"
	"strings"

	"git-compose/internal/cli"
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

func main() {
	app := &cli.App{
		Binary: "git-compose",
		EnvVars: []cli.EnvVar{
			{Name: "REPO_DIR", Description: "path to the homelab git repo", Default: defaultRepoDir},
			{Name: "CADDY_API", Description: "Caddy Admin API base URL", Default: defaultCaddyAPI},
			{Name: "SOPS_AGE_KEY_FILE", Description: "path to the SOPS age key file", Default: defaultSopsKeyFile},
			{Name: "GIT_DEPLOY_KEY", Description: "path to the SSH deploy key", Default: defaultDeployKeyFile},
			{Name: "CADDY_NET", Description: "Docker network name for Caddy", Default: defaultCaddyNet},
			{Name: "CADDY_POLL_ATTEMPTS", Description: "number of attempts to wait for Caddy API", Default: defaultPollAttempts},
			{Name: "CADDY_POLL_INTERVAL", Description: "interval between Caddy poll attempts, e.g. 2s", Default: defaultPollInterval},
		},
	}

	app.Register(&cli.Command{
		Name:        "reconcile",
		Usage:       "[--routes-only] [--changed-only] [repo-dir]",
		Description: "sync repo and deploy all services",
		Run: func(args []string) error {
			repoDir := envOr("REPO_DIR", defaultRepoDir)
			routesOnly := false
			changedOnly := false
			for _, arg := range args {
				switch arg {
				case "--routes-only":
					routesOnly = true
				case "--changed-only":
					changedOnly = true
				default:
					if !strings.HasPrefix(arg, "-") {
						repoDir = arg
					}
				}
			}
			return runReconcile(repoDir, routesOnly, changedOnly)
		},
	})

	app.Register(&cli.Command{
		Name:        "register-route",
		Usage:       "<caddy.json>",
		Description: "upsert a single Caddy route from a caddy.json file",
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("usage: git-compose register-route <caddy.json>")
			}
			caddyAPI := envOr("CADDY_API", defaultCaddyAPI)
			return runRegisterRoute(args[0], caddyAPI)
		},
	})

	app.Register(&cli.Command{
		Name:        "remove-route",
		Usage:       "<route-id>",
		Description: "delete a Caddy route by id",
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("usage: git-compose remove-route <route-id>")
			}
			caddyAPI := envOr("CADDY_API", defaultCaddyAPI)
			return runRemoveRoute(args[0], caddyAPI)
		},
	})

	app.Run(os.Args[1:])
}
