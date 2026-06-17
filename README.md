```
  _____ _ _    ___
 / ____(_) |  / __|
| |  __ _| |_| |    ___  _ __ ___  _ __   ___  ___  ___
| | |_ | | __| |   / _ \| '_ ` _ \| '_ \ / _ \/ __|/ _ \
| |__| | | |_| |__| (_) | | | | | | |_) | (_) \__ \  __/
 \_____|_|\__|\____\___/|_| |_| |_| .__/ \___/|___/\___|
                                   | |
                                   |_|
```

# git-compose

A GitOps reconciliation tool for homelab Docker Compose deployments. On each run it syncs a Git repository, deploys all Docker Compose services, manages encrypted secrets, configures Caddy as a reverse proxy, and self-updates from GitHub Releases — all in a single binary.

**Workflow:** commit to your homelab repo, run `git-compose reconcile`, everything is deployed.

---

## How it works

`git-compose reconcile` executes the following pipeline in order:

1. **Self-update** — fetches the latest GitHub Release; if newer, replaces the running binary and re-executes
2. **Git sync** — `git fetch origin main` + hard reset to track remote state
3. **Install git hooks** — copies files from `scripts/hooks/` into `.git/hooks/`
4. **Ensure Docker network** — creates the configured bridge network if it does not exist
5. **Deploy services** — for each `services/*/compose.yaml`, optionally decrypts secrets, then runs `docker compose up -d --remove-orphans --pull always`
6. **Apply Caddy routes** — reads all `services/*/caddy*.json`, assembles a full Caddy JSON config, and posts it to the Admin API
7. **Prune images** — removes dangling Docker images

Directories named `*.disabled` are silently skipped. Service errors are collected and reported together rather than short-circuiting the pipeline.

---

## Repository layout

`git-compose` expects the repository it manages to follow this structure:

```
/opt/homelab/
├── services/
│   ├── myapp/
│   │   ├── compose.yaml        # Docker Compose definition
│   │   ├── caddy.json          # Caddy route definition (optional)
│   │   └── secrets.enc.env     # SOPS-encrypted dotenv secrets (optional)
│   └── otherapp.disabled/      # Skipped — disabled by directory name convention
└── scripts/
    └── hooks/                  # Git hooks; auto-installed to .git/hooks/
```

### Caddy route files

Each `caddy*.json` file in a service directory defines a single reverse proxy route:

```json
{
  "id": "my-service",
  "hostname": "myapp.example.com",
  "upstream": "container-name:8080"
}
```

`git-compose` assembles these into a complete Caddy JSON config and applies it atomically via `POST /load`.

### Secrets

If `secrets.enc.env` exists in a service directory, `git-compose` will:

1. Decrypt it with [SOPS](https://github.com/getsops/sops) in `dotenv` format
2. Write the plaintext to `secrets.env` (mode `0600`)
3. Run `docker compose up` (which picks up `secrets.env` automatically)
4. Delete `secrets.env` immediately after, whether the deployment succeeded or failed

SOPS key resolution (AWS KMS, GCP KMS, Azure Key Vault, age, PGP, Vault) follows the standard SOPS config — no key paths are hardcoded.

---

## Installation

### Pre-built binary

Each push to `main` publishes a `git-compose-linux-arm64` binary to the `latest` GitHub Release. Download it and place it somewhere in your `PATH`:

```bash
curl -Lo /usr/local/bin/git-compose \
  https://github.com/<owner>/<repo>/releases/latest/download/git-compose-linux-arm64
chmod +x /usr/local/bin/git-compose
```

### Build from source

Requires Go 1.26.1+.

```bash
# Local build (current OS/arch, no self-update)
go build -o git-compose ./cmd/git-compose

# Production build (linux/arm64, stripped, with self-update support)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath \
  -ldflags="-s -w \
    -X main.githubRepo=owner/repo \
    -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o git-compose-linux-arm64 \
  ./cmd/git-compose
```

The `-X main.githubRepo=owner/repo` linker flag is required for self-update to function. Without it, auto-update is skipped with a warning.

---

## Usage

```
git-compose <command> [flags]
```

### Commands

#### `reconcile [<repo-dir>]`

Run the full reconcile pipeline.

```bash
git-compose reconcile [/path/to/homelab]
```

| Flag | Description |
|---|---|
| `--changed-only` | Only deploy services whose files changed since the last git sync |
| `--routes-only` | Skip git sync and Docker deployments; only re-apply Caddy routes |

#### `register-route <caddy-json>`

Upsert a single Caddy route from a `caddy.json` file.

```bash
git-compose register-route services/myapp/caddy.json
```

#### `remove-route <id>`

Remove a Caddy route by its ID.

```bash
git-compose remove-route my-service
```

#### `update`

Manually trigger a self-update.

```bash
git-compose update              # Update to latest release
git-compose update --check      # Check for updates without downloading
git-compose update --tag v1.2.3 # Update to a specific release tag
```

---

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|---|---|---|
| `REPO_DIR` | `/opt/homelab` | Path to the homelab git repo (overridden by the positional CLI arg) |
| `CADDY_API` | `http://127.0.0.1:2019` | Caddy Admin API base URL |
| `CADDY_NET` | `caddy-net` | Docker network shared between Caddy and services |
| `CADDY_ADMIN_LISTEN` | `:2019` | Admin listen address written into the generated Caddy config |
| `CADDY_HTTP_LISTEN` | `:80` | HTTP listen address written into the generated Caddy config |
| `CADDY_SERVER_NAME` | `srv0` | Server key in the generated Caddy config |
| `CADDY_POLL_ATTEMPTS` | `15` | Number of times to poll Caddy before giving up |
| `CADDY_POLL_INTERVAL` | `2s` | Duration between Caddy poll attempts (Go duration string) |

---

## CI/CD

The included GitHub Actions workflow (`.github/workflows/build.yml`) triggers on pushes to `main`, version tags (`v*`), and pull requests targeting `main`. It cross-compiles for `linux/arm64` with CGO disabled and attaches the binary to a `latest` GitHub Release, which the self-update mechanism polls.

---

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/alecthomas/kong` | CLI argument parsing |
| `github.com/docker/docker` | Docker SDK (network management, image pruning) |
| `github.com/fatih/color` | Colored terminal output |
| `github.com/getsops/sops/v3` | Secret decryption |

SOPS pulls in SDKs for all supported key providers (AWS KMS, GCP KMS, Azure Key Vault, age, PGP, HashiCorp Vault), which accounts for the large binary size (~40-60 MB stripped).
