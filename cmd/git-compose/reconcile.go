package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	gogit "github.com/go-git/go-git/v5"
	"github.com/getsops/sops/v3/decrypt"
)

func runReconcile(repoDir string, routesOnly bool, changedOnly bool) error {
	section("Reconciling homelab")

	caddyAPI := envOr("CADDY_API", defaultCaddyAPI)

	if !routesOnly {
		sopsKeyFile := envOr("SOPS_AGE_KEY_FILE", defaultSopsKeyFile)
		os.Setenv("SOPS_AGE_KEY_FILE", sopsKeyFile)

		// git sync — capture old HEAD so we can diff what changed
		oldHead, err := gitSync(repoDir)
		if err != nil {
			return fmt.Errorf("git sync: %w", err)
		}

		// install git hooks
		if err := installHooks(repoDir); err != nil {
			// non-fatal: warn and continue
			warn("installing hooks: %v", err)
		}

		// ensure caddy-net network
		caddyNet := envOr("CADDY_NET", defaultCaddyNet)
		if err := ensureNetwork(caddyNet); err != nil {
			warn("ensure network: %v", err)
		}

		// Determine which services to deploy.
		// When --changed-only is set, compute the diff old..new and only deploy
		// services whose files were touched. A nil filter means deploy everything.
		var serviceFilter map[string]bool
		if changedOnly {
			// Resolve current HEAD (post-sync).
			repo, openErr := gogit.PlainOpen(repoDir)
			if openErr != nil {
				return fmt.Errorf("open repo for diff: %w", openErr)
			}
			headRef, headErr := repo.Head()
			if headErr != nil {
				return fmt.Errorf("resolve HEAD after sync: %w", headErr)
			}
			newHead := headRef.Hash()

			if oldHead == newHead {
				info("no new commits; skipping service deployment")
			} else {
				var diffErr error
				serviceFilter, diffErr = changedServices(repoDir, oldHead, newHead)
				if diffErr != nil {
					warn("could not compute changed services, deploying all: %v", diffErr)
					serviceFilter = nil
				} else if len(serviceFilter) == 0 {
					info("no services changed; skipping service deployment")
				}
			}
		}

		// deploy services (serviceFilter == nil means deploy all)
		if !changedOnly || len(serviceFilter) > 0 {
			if err := deployServices(repoDir, serviceFilter); err != nil {
				// deployServices logs per-service failures and continues; a returned
				// error here means something structural failed.
				return fmt.Errorf("deploy services: %w", err)
			}
		}
	}

	// wait for Caddy Admin API
	if err := waitForCaddy(caddyAPI); err != nil {
		warn("Caddy API never became ready: %v", err)
	}

	// build + apply Caddy config
	if err := applyCaddyRoutes(repoDir, caddyAPI); err != nil {
		warn("applying Caddy routes: %v", err)
	}

	if !routesOnly {
		// prune old images
		pruneImages()
	}

	ok("Done")
	return nil
}

// deployServices finds all compose.yaml files under repoDir/services, and for
// each one: decrypts secrets if present, runs docker compose up, then removes
// the plaintext secrets file.
//
// When changedOnly is non-nil, only services whose name appears in the map are
// deployed; all others are silently skipped.
func deployServices(repoDir string, changedOnly map[string]bool) error {
	pattern := filepath.Join(repoDir, "services", "*", "compose.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(matches)

	for _, composeFile := range matches {
		dir := filepath.Dir(composeFile)
		service := filepath.Base(dir)

		if changedOnly != nil && !changedOnly[service] {
			continue
		}

		step("%s", service)

		secretsEnc := filepath.Join(dir, "secrets.env.enc")
		secretsPlain := filepath.Join(dir, "secrets.env")
		hasSecrets := false

		if _, statErr := os.Stat(secretsEnc); statErr == nil {
			hasSecrets = true
			data, decErr := decrypt.File(secretsEnc, "dotenv")
			if decErr != nil {
				fail("%s: failed to decrypt secrets, skipping", service)
				continue
			}
			if writeErr := os.WriteFile(secretsPlain, data, 0o600); writeErr != nil {
				fail("%s: failed to write secrets.env, skipping", service)
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
			fail("%s: failed to deploy, skipping", service)
		}
	}
	return nil
}

