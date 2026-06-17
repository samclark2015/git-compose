package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

// gitSync fetches and hard-resets to origin/main using go-git (no git binary required).
// It returns the HEAD hash that was current before the sync (the "old" HEAD), which
// callers can use to determine what changed.
func gitSync(repoDir string) (plumbing.Hash, error) {
	step("Syncing git")

	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("open repo: %w", err)
	}

	// Capture HEAD before the sync so callers can diff old..new.
	headRef, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve HEAD: %w", err)
	}
	oldHead := headRef.Hash()

	deployKeyFile := envOr("GIT_DEPLOY_KEY", defaultDeployKeyFile)

	// Build SSH auth from the deploy key file
	auth, err := gitssh.NewPublicKeysFromFile("git", deployKeyFile, "")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load deploy key %s: %w", deployKeyFile, err)
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
		return plumbing.ZeroHash, fmt.Errorf("git fetch: %w", fetchErr)
	}

	// Resolve origin/main
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve origin/main: %w", err)
	}

	// Hard reset worktree to that commit
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Commit: ref.Hash(),
		Mode:   gogit.HardReset,
	}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git reset --hard: %w", err)
	}

	ok("HEAD reset to %s", ref.Hash())
	return oldHead, nil
}

// changedServices returns the set of service names (directory names under
// services/) that have at least one file changed between oldHead and newHead.
// If the two hashes are identical (already up-to-date), it returns nil, which
// callers interpret as "nothing changed".
func changedServices(repoDir string, oldHead, newHead plumbing.Hash) (map[string]bool, error) {
	if oldHead == newHead {
		return nil, nil
	}

	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}

	oldCommit, err := repo.CommitObject(oldHead)
	if err != nil {
		return nil, fmt.Errorf("resolve old commit %s: %w", oldHead, err)
	}
	newCommit, err := repo.CommitObject(newHead)
	if err != nil {
		return nil, fmt.Errorf("resolve new commit %s: %w", newHead, err)
	}

	oldTree, err := oldCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("old tree: %w", err)
	}
	newTree, err := newCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("new tree: %w", err)
	}

	changes, err := oldTree.Diff(newTree)
	if err != nil {
		return nil, fmt.Errorf("diff trees: %w", err)
	}

	changed := make(map[string]bool)
	for _, ch := range changes {
		// A change has a From and a To path; use whichever is non-empty.
		for _, p := range []string{ch.From.Name, ch.To.Name} {
			if p == "" {
				continue
			}
			// Match paths like "services/<name>/..."
			parts := strings.SplitN(p, "/", 3)
			if len(parts) >= 2 && parts[0] == "services" && parts[1] != "" {
				changed[parts[1]] = true
			}
		}
	}
	return changed, nil
}

// installHooks copies every file from scripts/hooks/ into .git/hooks/.
func installHooks(repoDir string) error {
	step("Installing git hooks")
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
		ok("installed %s", e.Name())
	}
	return nil
}
