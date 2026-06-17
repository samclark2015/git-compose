package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git-compose/internal/ui"
)

// ---------------------------------------------------------------------------
// Caddy config types
// ---------------------------------------------------------------------------

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

// routeWithID is the payload sent to Caddy when registering a named route.
type routeWithID struct {
	ID       string        `json:"@id"`
	Match    []caddyMatch  `json:"match"`
	Handle   []caddyHandle `json:"handle"`
	Terminal bool          `json:"terminal"`
}

// liveRoute is used only for parsing GET /config/.../routes to find @id values.
type liveRoute struct {
	ID string `json:"@id"`
}

// caddyJSON is the schema of each services/*/caddy*.json file.
type caddyJSON struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Upstream string `json:"upstream"`
}

// ---------------------------------------------------------------------------
// poll helpers
// ---------------------------------------------------------------------------

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
	ui.Step("Waiting for Caddy Admin API")
	poll := parsePollConfig()
	url := caddyAPI + "/config/"
	for i := 1; i <= poll.attempts; i++ {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				ui.OK("Caddy ready")
				return nil
			}
		}
		ui.Info("waiting (%d/%d)...", i, poll.attempts)
		time.Sleep(poll.interval)
	}
	return fmt.Errorf("timed out waiting for Caddy")
}

// applyCaddyRoutes reconciles managed Caddy routes using the per-route @id
// API. It leaves all unmanaged routes (static Caddyfile entries, etc.) intact.
//
// Algorithm:
//  1. Collect the set of IDs from the current caddy*.json files.
//  2. GET the live routes array; delete any managed route whose ID is no
//     longer in the active set (stale cleanup).
//  3. Upsert every active route via DELETE-then-POST.
func applyCaddyRoutes(repoDir, caddyAPI string) error {
	ui.Step("Applying Caddy routes via API")

	pattern := filepath.Join(repoDir, "services", "*", "caddy*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(matches)

	// Build the set of active caddyJSON entries, keyed by ID.
	active := map[string]caddyJSON{}
	for _, f := range matches {
		if strings.HasSuffix(filepath.Base(filepath.Dir(f)), ".disabled") {
			continue
		}
		data, readErr := os.ReadFile(f)
		if readErr != nil {
			ui.Warn("reading %s: %v", f, readErr)
			continue
		}
		var cj caddyJSON
		if jsonErr := json.Unmarshal(data, &cj); jsonErr != nil {
			ui.Warn("parsing %s: %v", f, jsonErr)
			continue
		}
		active[cj.ID] = cj
	}

	caddyServerName := envOr("CADDY_SERVER_NAME", "srv0")
	caddyHTTPListen := envOr("CADDY_HTTP_LISTEN", ":80")

	// Ensure the server node exists in the JSON config tree before we try to
	// append routes to it. When Caddy starts from a Caddyfile the adapter
	// manages config internally and the JSON path may not be traversable.
	if err := ensureCaddyServer(caddyAPI, caddyServerName, caddyHTTPListen); err != nil {
		return fmt.Errorf("ensuring Caddy server: %w", err)
	}

	// Fetch live routes and delete any that carry one of our IDs but are no
	// longer in the active set.
	if err := pruneStaleRoutes(caddyAPI, caddyServerName, active); err != nil {
		ui.Warn("pruning stale routes: %v", err)
	}

	// Upsert every active route.
	var errs []error
	for _, cj := range active {
		if err := upsertCaddyRoute(cj, caddyAPI, caddyServerName); err != nil {
			ui.Warn("upserting route %s: %v", cj.ID, err)
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("applyCaddyRoutes: %d route(s) failed", len(errs))
	}

	ui.OK("Caddy routes applied (%d route(s))", len(active))
	return nil
}

// ensureCaddyServer checks whether the named server exists in Caddy's JSON
// config tree and creates it (with an empty routes array) if it does not.
// This is necessary when Caddy starts from a Caddyfile: the adapter manages
// the config internally and the apps/http subtree may not be traversable via
// the config API until we seed it.
func ensureCaddyServer(caddyAPI, serverName, listenAddr string) error {
	serverURL := caddyAPI + "/config/apps/http/servers/" + serverName
	resp, err := http.Get(serverURL) //nolint:noctx
	if err != nil {
		return fmt.Errorf("GET %s: %w", serverURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil // already exists
	}

	// Server path doesn't exist (or config/apps/http doesn't exist yet).
	// Seed the full apps/http subtree so subsequent route POSTs succeed.
	type serverNode struct {
		Listen []string `json:"listen"`
		Routes []any    `json:"routes"`
	}
	type httpNode struct {
		Servers map[string]serverNode `json:"servers"`
	}
	seed, _ := json.Marshal(httpNode{
		Servers: map[string]serverNode{
			serverName: {Listen: []string{listenAddr}, Routes: []any{}},
		},
	})

	appsHTTPURL := caddyAPI + "/config/apps/http"
	putResp, err := doPut(appsHTTPURL, seed)
	if err != nil {
		return err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("PUT %s returned %d: %s", appsHTTPURL, putResp.StatusCode, strings.TrimSpace(string(body)))
	}
	ui.Info("Seeded Caddy server %q", serverName)
	return nil
}

// doPut issues an HTTP PUT with a JSON body.
func doPut(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req) //nolint:noctx
}

// pruneStaleRoutes GETs the live routes for serverName, finds any whose @id
// is not in the active set, and deletes them.
func pruneStaleRoutes(caddyAPI, serverName string, active map[string]caddyJSON) error {
	url := caddyAPI + "/config/apps/http/servers/" + serverName + "/routes"
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Server not yet configured — nothing to prune.
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var live []liveRoute
	if err := json.Unmarshal(body, &live); err != nil {
		return fmt.Errorf("parsing live routes: %w", err)
	}

	for _, r := range live {
		if r.ID == "" {
			continue // unmanaged route — skip
		}
		if _, ok := active[r.ID]; ok {
			continue // still active — will be upserted
		}
		// Stale managed route — delete it.
		delURL := caddyAPI + "/id/" + r.ID
		req, _ := http.NewRequest(http.MethodDelete, delURL, nil)
		delResp, delErr := http.DefaultClient.Do(req) //nolint:noctx
		if delErr != nil {
			ui.Warn("DELETE stale route %s: %v", r.ID, delErr)
			continue
		}
		delResp.Body.Close()
		ui.Info("Removed stale route: %s", r.ID)
	}
	return nil
}

// upsertCaddyRoute deletes any existing route with cj.ID then POSTs the new
// route definition. This is idempotent and leaves all other routes untouched.
func upsertCaddyRoute(cj caddyJSON, caddyAPI, serverName string) error {
	// Delete existing (idempotent; 404 is fine).
	delURL := caddyAPI + "/id/" + cj.ID
	req, _ := http.NewRequest(http.MethodDelete, delURL, nil)
	delResp, err := http.DefaultClient.Do(req) //nolint:noctx
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", delURL, err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE %s returned %d", delURL, delResp.StatusCode)
	}

	route := routeWithID{
		ID:    cj.ID,
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
		Terminal: true,
	}
	payload, err := json.Marshal(route)
	if err != nil {
		return err
	}

	postURL := caddyAPI + "/config/apps/http/servers/" + serverName + "/routes/"
	postResp, err := http.Post(postURL, "application/json", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("POST %s: %w", postURL, err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(postResp.Body)
		return fmt.Errorf("POST %s returned %d: %s", postURL, postResp.StatusCode, strings.TrimSpace(string(body)))
	}
	ui.Info("Upserted route: %s → %s", cj.Hostname, cj.Upstream)
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

	ui.Info("Registering %s → %s (id: %s)", cj.Hostname, cj.Upstream, cj.ID)

	caddyServerName := envOr("CADDY_SERVER_NAME", "srv0")
	if err := upsertCaddyRoute(cj, caddyAPI, caddyServerName); err != nil {
		return err
	}
	ui.OK("Route registered")
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
	ui.OK("Removed route: %s", routeID)
	return nil
}
