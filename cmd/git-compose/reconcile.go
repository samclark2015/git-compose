package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git-compose/internal/ui"

	"github.com/getsops/sops/v3/decrypt"
)

func runReconcile(repoDir string, routesOnly bool, changedOnly bool) error {
	ui.Section("Reconciling homelab")

	// Auto-update: fetch the latest release and re-exec if a newer binary was
	// installed. Non-fatal: a warning is printed and reconcile continues if the
	// update fails (e.g. no network, no matching asset).
	if err := selfUpdate(); err != nil {
		ui.Warn("auto-update failed: %v", err)
	}

	caddyAPI := envOr("CADDY_API", defaultCaddyAPI)

	if !routesOnly {
		// git sync — capture old HEAD so we can diff what changed
		oldHead, err := gitSync(repoDir)
		if err != nil {
			return fmt.Errorf("git sync: %w", err)
		}

		// install git hooks
		if err := installHooks(repoDir); err != nil {
			// non-fatal: warn and continue
			ui.Warn("installing hooks: %v", err)
		}

		// ensure caddy-net network
		caddyNet := envOr("CADDY_NET", defaultCaddyNet)
		if err := ensureNetwork(caddyNet); err != nil {
			ui.Warn("ensure network: %v", err)
		}

		// Determine which services to deploy.
		// When --changed-only is set, compute the diff old..new and only deploy
		// services whose files were touched. A nil filter means deploy everything.
		var serviceFilter map[string]bool
		if changedOnly {
			newHead, headErr := runOutput(repoDir, "git", "rev-parse", "HEAD")
			if headErr != nil {
				return fmt.Errorf("resolve HEAD after sync: %w", headErr)
			}

			if oldHead == newHead {
				ui.Info("no new commits; skipping service deployment")
			} else {
				var diffErr error
				serviceFilter, diffErr = changedServices(repoDir, oldHead, newHead)
				if diffErr != nil {
					ui.Warn("could not compute changed services, deploying all: %v", diffErr)
					serviceFilter = nil
				} else if len(serviceFilter) == 0 {
					ui.Info("no services changed; skipping service deployment")
				}
			}
		}

		// deploy services (serviceFilter == nil means deploy all)
		if !changedOnly || len(serviceFilter) > 0 {
			if err := deployServices(repoDir, serviceFilter); err != nil {
				return fmt.Errorf("deploy services: %w", err)
			}
		}
	}

	// wait for Caddy Admin API
	if err := waitForCaddy(caddyAPI); err != nil {
		ui.Warn("Caddy API never became ready: %v", err)
	}

	// build + apply Caddy config
	if err := applyCaddyRoutes(repoDir, caddyAPI); err != nil {
		ui.Warn("applying Caddy routes: %v", err)
	}

	if !routesOnly {
		// prune old images
		pruneImages()
	}

	ui.OK("Done")
	return nil
}

// deployServices finds all compose.yaml files under repoDir/services, and for
// each one: decrypts secrets if present, runs docker compose up, then removes
// the plaintext secrets file.
//
// When changedOnly is non-nil, only services whose name appears in the map are
// deployed; all others are silently skipped.
//
// All services are attempted regardless of individual failures. Any per-service
// errors are joined and returned together so the caller can report and fail on
// a partial deployment.
func deployServices(repoDir string, changedOnly map[string]bool) error {
	pattern := filepath.Join(repoDir, "services", "*", "compose.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(matches)

	var errs []error

	for _, composeFile := range matches {
		dir := filepath.Dir(composeFile)
		service := filepath.Base(dir)

		if strings.HasSuffix(service, ".disabled") {
			ui.Info("%s: disabled (skipping)", service)
			continue
		}

		if changedOnly != nil && !changedOnly[service] {
			continue
		}

		ui.Step("%s", service)

		secretsEnc := filepath.Join(dir, "secrets.enc.env")
		secretsPlain := filepath.Join(dir, "secrets.env")
		hasSecrets := false

		if _, statErr := os.Stat(secretsEnc); statErr == nil {
			hasSecrets = true
			data, decErr := decrypt.File(secretsEnc, "dotenv")
			if decErr != nil {
				ui.Fail("%s: failed to decrypt secrets", service)
				errs = append(errs, fmt.Errorf("%s: decrypt secrets: %w", service, decErr))
				continue
			}
			if writeErr := os.WriteFile(secretsPlain, data, 0o600); writeErr != nil {
				ui.Fail("%s: failed to write secrets.env", service)
				errs = append(errs, fmt.Errorf("%s: write secrets.env: %w", service, writeErr))
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
			ui.Fail("%s: failed to deploy", service)
			errs = append(errs, fmt.Errorf("%s: docker compose up: %w", service, upErr))
		} else {
			ui.OK("%s", service)
		}
	}

	return errors.Join(errs...)
}
