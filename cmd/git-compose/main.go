package main

import (
	"github.com/alecthomas/kong"
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
// command structs
// ---------------------------------------------------------------------------

type reconcileCmd struct {
	RepoDir     string `arg:"" optional:"" default:"/opt/homelab" env:"REPO_DIR" help:"Path to the homelab git repo."`
	RoutesOnly  bool   `name:"routes-only"  help:"Skip git sync, services, and image prune; only apply Caddy routes."`
	ChangedOnly bool   `name:"changed-only" help:"Only deploy services whose files changed since the last reconcile."`
}

func (c *reconcileCmd) Run() error {
	return runReconcile(c.RepoDir, c.RoutesOnly, c.ChangedOnly)
}

type registerRouteCmd struct {
	CaddyJSON string `arg:"" name:"caddy.json" help:"Path to the caddy.json file."`
	CaddyAPI  string `env:"CADDY_API" default:"http://127.0.0.1:2019" help:"Caddy Admin API base URL."`
}

func (c *registerRouteCmd) Run() error {
	return runRegisterRoute(c.CaddyJSON, c.CaddyAPI)
}

type removeRouteCmd struct {
	RouteID  string `arg:"" name:"route-id" help:"ID of the Caddy route to delete."`
	CaddyAPI string `env:"CADDY_API" default:"http://127.0.0.1:2019" help:"Caddy Admin API base URL."`
}

func (c *removeRouteCmd) Run() error {
	return runRemoveRoute(c.RouteID, c.CaddyAPI)
}

// ---------------------------------------------------------------------------
// root CLI
// ---------------------------------------------------------------------------

var cli struct {
	Reconcile     reconcileCmd     `cmd:"" help:"Sync repo and deploy all services."`
	RegisterRoute registerRouteCmd `cmd:"" name:"register-route" help:"Upsert a single Caddy route from a caddy.json file."`
	RemoveRoute   removeRouteCmd   `cmd:"" name:"remove-route"   help:"Delete a Caddy route by id."`
	Update        updateCmd        `cmd:"" help:"Update git-compose to the latest GitHub release."`
}

func main() {
	ctx := kong.Parse(&cli,
		kong.Name("git-compose"),
		kong.Description("GitOps deployment tool for homelab services."),
		kong.UsageOnError(),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
