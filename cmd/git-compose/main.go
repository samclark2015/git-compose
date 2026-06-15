package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/getsops/sops/v3/decrypt"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

const (
	defaultRepoDir    = "/opt/homelab"
	defaultCaddyAPI   = "http://127.0.0.1:2019"
	defaultSopsKeyFile   = "/root/.config/sops/age/keys.txt"
	defaultDeployKeyFile = "/root/.ssh/gitops_deploy"
	defaultCaddyNet      = "caddy-net"
	defaultPollAttempts  = "15"
	defaultPollInterval  = "2s"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "reconcile":
		repoDir := envOr("REPO_DIR", defaultRepoDir)
		routesOnly := false
		for _, arg := range os.Args[2:] {
			if arg == "--routes-only" {
				routesOnly = true
			} else if !strings.HasPrefix(arg, "-") {
				repoDir = arg
			}
		}
		if err := runReconcile(repoDir, routesOnly); err != nil {
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
  reconcile [--routes-only] [repo-dir]  sync repo and deploy all services (default repo: /opt/homelab)
                                        --routes-only: skip git sync, services, and image prune; only apply Caddy routes
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

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

func runReconcile(repoDir string, routesOnly bool) error {
	fmt.Println("=== Reconciling homelab ===")

	caddyAPI := envOr("CADDY_API", defaultCaddyAPI)

	if !routesOnly {
		sopsKeyFile := envOr("SOPS_AGE_KEY_FILE", defaultSopsKeyFile)
		os.Setenv("SOPS_AGE_KEY_FILE", sopsKeyFile)

		// git sync
		if err := gitSync(repoDir); err != nil {
			return fmt.Errorf("git sync: %w", err)
		}

		// install git hooks
		if err := installHooks(repoDir); err != nil {
			// non-fatal: warn and continue
			fmt.Fprintf(os.Stderr, "WARNING: installing hooks: %v\n", err)
		}

		// ensure caddy-net network
		caddyNet := envOr("CADDY_NET", defaultCaddyNet)
		if err := ensureNetwork(caddyNet); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: ensure network: %v\n", err)
		}

		// deploy services
		if err := deployServices(repoDir); err != nil {
			// deployServices logs per-service failures and continues; a returned
			// error here means something structural failed.
			return fmt.Errorf("deploy services: %w", err)
		}
	}

	// wait for Caddy Admin API
	if err := waitForCaddy(caddyAPI); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Caddy API never became ready: %v\n", err)
	}

	// build + apply Caddy config
	if err := applyCaddyRoutes(repoDir, caddyAPI); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: applying Caddy routes: %v\n", err)
	}

	if !routesOnly {
		// prune old images
		pruneImages()
	}

	fmt.Println("=== Done ===")
	return nil
}

// gitSync fetches and hard-resets to origin/main using go-git (no git binary required).
func gitSync(repoDir string) error {
	fmt.Println("--- Syncing git ---")

	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	deployKeyFile := envOr("GIT_DEPLOY_KEY", defaultDeployKeyFile)

	// Build SSH auth from the deploy key file
	auth, err := gitssh.NewPublicKeysFromFile("git", deployKeyFile, "")
	if err != nil {
		return fmt.Errorf("load deploy key %s: %w", deployKeyFile, err)
	}
	auth.HostKeyCallback = cryptossh.InsecureIgnoreHostKey() //nolint:gosec

	// Fetch origin/main
	fetchErr := repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/heads/main:refs/remotes/origin/main"},
		Auth:       auth,
		Progress:   os.Stdout,
	})
	if fetchErr != nil && !errors.Is(fetchErr, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git fetch: %w", fetchErr)
	}

	// Resolve origin/main
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	if err != nil {
		return fmt.Errorf("resolve origin/main: %w", err)
	}

	// Hard reset worktree to that commit
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Commit: ref.Hash(),
		Mode:   gogit.HardReset,
	}); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}

	fmt.Printf("  HEAD reset to %s\n", ref.Hash())
	return nil
}

// installHooks copies every file from scripts/hooks/ into .git/hooks/.
func installHooks(repoDir string) error {
	fmt.Println("--- Installing git hooks ---")
	hooksDir := filepath.Join(repoDir, "scripts", "hooks")
	entries, err := os.ReadDir(hooksDir)
	if os.IsNotExist(err) {
		return nil // no hooks directory — skip silently
	}
	if err != nil {
		return err
	}
	destDir := filepath.Join(repoDir, ".git", "hooks")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(hooksDir, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy hook %s: %w", e.Name(), err)
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			return err
		}
		fmt.Printf("  installed %s\n", e.Name())
	}
	return nil
}

// ensureNetwork creates the named Docker bridge network if it doesn't exist.
func ensureNetwork(name string) error {
	fmt.Printf("--- Ensuring %s network ---\n", name)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	ctx := context.Background()
	_, err = cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		fmt.Printf("  %s network already exists\n", name)
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("network inspect: %w", err)
	}

	_, err = cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		fmt.Printf("  WARNING: failed to create %s network\n", name)
		return err
	}
	fmt.Printf("  %s network created\n", name)
	return nil
}

// pruneImages removes dangling Docker images (equivalent to docker image prune -f).
func pruneImages() {
	fmt.Println("--- Pruning dangling images ---")

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintf(os.Stderr, "  WARNING: docker client for prune: %v\n", err)
		return
	}
	defer cli.Close()

	report, err := cli.ImagesPrune(context.Background(), filters.Args{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  WARNING: image prune: %v\n", err)
		return
	}
	fmt.Printf("  reclaimed %d bytes across %d image(s)\n", report.SpaceReclaimed, len(report.ImagesDeleted))
}

// deployServices finds all compose.yaml files under repoDir/services, and for
// each one: decrypts secrets if present, runs docker compose up, then removes
// the plaintext secrets file.
func deployServices(repoDir string) error {
	pattern := filepath.Join(repoDir, "services", "*", "compose.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(matches)

	for _, composeFile := range matches {
		dir := filepath.Dir(composeFile)
		service := filepath.Base(dir)
		fmt.Printf("--- %s ---\n", service)

		secretsEnc := filepath.Join(dir, "secrets.env.enc")
		secretsPlain := filepath.Join(dir, "secrets.env")
		hasSecrets := false

		if _, statErr := os.Stat(secretsEnc); statErr == nil {
			hasSecrets = true
			data, decErr := decrypt.File(secretsEnc, "dotenv")
			if decErr != nil {
				fmt.Printf("!!! %s: failed to decrypt secrets, skipping\n", service)
				continue
			}
			if writeErr := os.WriteFile(secretsPlain, data, 0o600); writeErr != nil {
				fmt.Printf("!!! %s: failed to write secrets.env, skipping\n", service)
				continue
			}
		}

		upErr := run("", "docker", "compose",
			"-f", composeFile,
			"--project-name", service,
			"up", "-d", "--remove-orphans", "--pull", "always",
		)
		if hasSecrets {
			os.Remove(secretsPlain)
		}
		if upErr != nil {
			fmt.Printf("!!! %s failed to deploy, skipping\n", service)
		}
	}
	return nil
}

// pollConfig holds parsed poll settings for waitForCaddy.
type pollConfig struct {
	attempts int
	interval time.Duration
}

// parsePollConfig reads CADDY_POLL_ATTEMPTS and CADDY_POLL_INTERVAL from the
// environment, falling back to the defaults on any parse error.
func parsePollConfig() pollConfig {
	cfg := pollConfig{attempts: 15, interval: 2 * time.Second}

	if v := os.Getenv("CADDY_POLL_ATTEMPTS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.attempts = n
		}
	}
	if v := os.Getenv("CADDY_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.interval = d
		}
	}
	return cfg
}

// waitForCaddy polls the Caddy Admin API until it responds or the attempt
// limit is reached.
func waitForCaddy(caddyAPI string) error {
	fmt.Println("--- Waiting for Caddy Admin API ---")
	poll := parsePollConfig()
	url := caddyAPI + "/config/"
	for i := 1; i <= poll.attempts; i++ {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				fmt.Println("  Caddy ready")
				return nil
			}
		}
		fmt.Printf("  waiting (%d/%d)...\n", i, poll.attempts)
		time.Sleep(poll.interval)
	}
	return fmt.Errorf("timed out waiting for Caddy")
}

// ---------------------------------------------------------------------------
// Caddy config types
// ---------------------------------------------------------------------------

type caddyConfig struct {
	Admin caddyAdmin `json:"admin"`
	Apps  caddyApps  `json:"apps"`
}

type caddyAdmin struct {
	Listen string `json:"listen"`
}

type caddyApps struct {
	HTTP caddyHTTP `json:"http"`
}

type caddyHTTP struct {
	Servers map[string]caddyServer `json:"servers"`
}

type caddyServer struct {
	Listen []string     `json:"listen"`
	Routes []caddyRoute `json:"routes"`
}

type caddyRoute struct {
	Match  []caddyMatch  `json:"match"`
	Handle []caddyHandle `json:"handle"`
}

type caddyMatch struct {
	Host []string `json:"host"`
}

type caddyHandle struct {
	Handler   string          `json:"handler"`
	Upstreams []caddyUpstream `json:"upstreams"`
	Headers   *caddyHeaders   `json:"headers,omitempty"`
}

type caddyUpstream struct {
	Dial string `json:"dial"`
}

type caddyHeaders struct {
	Request *caddyHeaderOps `json:"request,omitempty"`
}

type caddyHeaderOps struct {
	Set map[string][]string `json:"set,omitempty"`
}

// caddyJSON is the schema of each services/*/caddy*.json file.
type caddyJSON struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Upstream string `json:"upstream"`
}

// applyCaddyRoutes reads all caddy*.json files under services/, builds a full
// Caddy config, and pushes it to the Caddy Admin API via POST /load.
func applyCaddyRoutes(repoDir, caddyAPI string) error {
	fmt.Println("--- Applying Caddy routes via API ---")

	pattern := filepath.Join(repoDir, "services", "*", "caddy*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(matches)

	var routes []caddyRoute
	for _, f := range matches {
		data, readErr := os.ReadFile(f)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: reading %s: %v\n", f, readErr)
			continue
		}
		var cj caddyJSON
		if jsonErr := json.Unmarshal(data, &cj); jsonErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: parsing %s: %v\n", f, jsonErr)
			continue
		}
		route := caddyRoute{
			Match: []caddyMatch{{Host: []string{cj.Hostname}}},
			Handle: []caddyHandle{{
				Handler:   "reverse_proxy",
				Upstreams: []caddyUpstream{{Dial: cj.Upstream}},
				Headers: &caddyHeaders{
					Request: &caddyHeaderOps{
						Set: map[string][]string{
							"X-Forwarded-Proto": {"https"},
						},
					},
				},
			}},
		}
		routes = append(routes, route)
	}

	caddyAdminListen := envOr("CADDY_ADMIN_LISTEN", ":2019")
	caddyHTTPListen := envOr("CADDY_HTTP_LISTEN", ":80")
	caddyServerName := envOr("CADDY_SERVER_NAME", "srv0")

	cfg := caddyConfig{
		Admin: caddyAdmin{Listen: caddyAdminListen},
		Apps: caddyApps{
			HTTP: caddyHTTP{
				Servers: map[string]caddyServer{
					caddyServerName: {
						Listen: []string{caddyHTTPListen},
						Routes: routes,
					},
				},
			},
		},
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	resp, err := http.Post(caddyAPI+"/load", "application/json", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("POST /load: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /load returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Println("  Caddy routes applied")
	return nil
}

// ---------------------------------------------------------------------------
// register-route
// ---------------------------------------------------------------------------

func runRegisterRoute(caddyJSONPath, caddyAPI string) error {
	data, err := os.ReadFile(caddyJSONPath)
	if err != nil {
		return err
	}
	var cj caddyJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return fmt.Errorf("parsing %s: %w", caddyJSONPath, err)
	}

	fmt.Printf("Registering %s → %s (id: %s)\n", cj.Hostname, cj.Upstream, cj.ID)

	// Delete existing route (idempotent; ignore 404)
	delURL := caddyAPI + "/id/" + cj.ID
	req, _ := http.NewRequest(http.MethodDelete, delURL, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", delURL, err)
	}
	delResp.Body.Close()
	switch delResp.StatusCode {
	case http.StatusOK:
		fmt.Println("  Removed existing route")
	case http.StatusNotFound:
		fmt.Println("  No existing route found")
	default:
		return fmt.Errorf("unexpected DELETE status: %d", delResp.StatusCode)
	}

	// Build route object with @id for future deletion
	type routeWithID struct {
		ID       string        `json:"@id"`
		Match    []caddyMatch  `json:"match"`
		Handle   []caddyHandle `json:"handle"`
		Terminal bool          `json:"terminal"`
	}

	caddyServerName := envOr("CADDY_SERVER_NAME", "srv0")

	route := routeWithID{
		ID:    cj.ID,
		Match: []caddyMatch{{Host: []string{cj.Hostname}}},
		Handle: []caddyHandle{{
			Handler:   "reverse_proxy",
			Upstreams: []caddyUpstream{{Dial: cj.Upstream}},
		}},
		Terminal: true,
	}
	payload, err := json.Marshal(route)
	if err != nil {
		return err
	}

	postURL := caddyAPI + "/config/apps/http/servers/" + caddyServerName + "/routes/"
	postResp, err := http.Post(postURL, "application/json", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("POST %s: %w", postURL, err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(postResp.Body)
		return fmt.Errorf("unexpected POST status %d: %s", postResp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Println("  Created route")
	return nil
}

// ---------------------------------------------------------------------------
// remove-route
// ---------------------------------------------------------------------------

func runRemoveRoute(routeID, caddyAPI string) error {
	url := caddyAPI + "/id/" + routeID
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", url, err)
	}
	defer resp.Body.Close()
	fmt.Printf("Removed route: %s\n", routeID)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// run executes a command with its stdout/stderr wired to the current process.
// dir may be empty to inherit the current working directory.
func run(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// envOr returns the value of the environment variable key, or fallback if unset.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// copyFile copies src to dst, creating dst if it doesn't exist.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
