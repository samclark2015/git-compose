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

// applyCaddyRoutes reads all caddy*.json files under services/, builds a full
// Caddy config, and pushes it to the Caddy Admin API via POST /load.
func applyCaddyRoutes(repoDir, caddyAPI string) error {
	ui.Step("Applying Caddy routes via API")

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
			ui.Warn("reading %s: %v", f, readErr)
			continue
		}
		var cj caddyJSON
		if jsonErr := json.Unmarshal(data, &cj); jsonErr != nil {
			ui.Warn("parsing %s: %v", f, jsonErr)
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
	ui.OK("Caddy routes applied (%d route(s))", len(routes))
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
		ui.OK("Removed existing route")
	case http.StatusNotFound:
		ui.Info("No existing route found")
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
	ui.OK("Created route")
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
