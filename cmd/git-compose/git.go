package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git-compose/internal/ui"
)

// gitSync fetches and hard-resets to origin/main using the system git binary.
// SSH authentication is left entirely to the system (SSH agent, ~/.ssh/config,
// GIT_SSH_COMMAND, etc.). It returns the HEAD hash that was current before the
// sync so callers can diff old..new.
func gitSync(repoDir string) (string, error) {
	ui.Step("Syncing git")

	oldHead, err := runOutput(repoDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	if err := run(repoDir, "git", "fetch", "origin", "main"); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}
	if err := run(repoDir, "git", "reset", "--hard", "origin/main"); err != nil {
		return "", fmt.Errorf("git reset --hard: %w", err)
	}

	newHead, err := runOutput(repoDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD after sync: %w", err)
	}

	ui.OK("HEAD reset to %s", newHead)
	return oldHead, nil
}

// changedServices returns the set of service names (directory names under
// services/) that have at least one file changed between oldHead and newHead.
// Returns nil if the hashes are identical (nothing changed).
func changedServices(repoDir, oldHead, newHead string) (map[string]bool, error) {
	if oldHead == newHead {
		return nil, nil
	}

	out, err := runOutput(repoDir, "git", "diff", "--name-only", oldHead, newHead)
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	changed := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "/", 3)
		if len(parts) >= 2 && parts[0] == "services" && parts[1] != "" {
			changed[parts[1]] = true
		}
	}
	return changed, nil
}

// installHooks copies every file from scripts/hooks/ into .git/hooks/.
func installHooks(repoDir string) error {
	ui.Step("Installing git hooks")
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
		ui.OK("installed %s", e.Name())
	}
	return nil
}
