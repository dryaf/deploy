# Deploy Tool 🚀

**The "Platform Engineering in a Box" solution for Go applications on Linux VPS.**

This tool replaces fragile Makefiles and complex Bash scripts with a single, declarative CLI. It leverages modern Linux standards (**Podman**, **Quadlets**, **Systemd**) to provide zero-downtime deployments, automatic rollbacks, and integrated infrastructure management.

---

## ✨ Key Features

*   **Declarative Infrastructure:** Define your environments (Prod, Test), build settings, and runtime configuration in one `deploy.yaml`.
*   **Quadlet Native:** Automatically generates modern Systemd unit files for Podman, ensuring your containers start at boot and restart on failure.
*   **Safety First:**
    *   **Atomic Deployments:** Uses a blue-green style switch over via Systemd.
    *   **Auto-Rollback:** If the new version fails health checks, the tool automatically restores the previous binary and restarts the service.
    *   **Dry Run:** Preview every shell command before it executes.
*   **Infrastructure Management:**
    *   **Traefik Bootstrap:** Installs and configures Traefik (with Let's Encrypt) on a fresh server with one command.
    *   **Label Abstraction:** Generates complex Traefik labels (Auth, Rate Limits, Middleware) from simple YAML config.
*   **Developer Experience:**
    *   **Log Streaming:** Tail logs locally without SSH-ing into the server.
    *   **Database Sync:** Pull production SQLite databases to local or push local state to staging environments.
    *   **SSH Identity:** Full support for specific identity keys (`-i ~/.ssh/key`).
*   **Distroless Ready:** Built-in support for `podman unshare` to manage volume permissions for non-root containers (UID 65532).

---

## 📦 Installation

1.  Clone this repository.
2.  Run `make install` (installs to `~/bin`).
3.  Ensure `~/bin` is in your `$PATH`.

```bash
git clone <repo-url> deploy-tool
cd deploy-tool
make install
deploy --help
```

---

## 📖 Configuration (`deploy.yaml`)

Run `deploy init` to generate a starter file, or use this reference to configure every aspect of your deployment.

```yaml
# ==============================================================================
# GLOBAL SETTINGS
# ==============================================================================
app_name: "my-awesome-app"
binary_name: "server" # The name of the compiled binary

# Build Configuration
# Defines how the Go binary is compiled locally before upload.
build:
  arch: "amd64" # Target architecture (amd64, arm64)
  # LDFLAGS template for version injection.
  # Available variables: {{.Version}}, {{.Date}}, {{.Tag}}, {{.Commit}}
  ldflags: "-s -w -X 'main.Version={{.Version}}' -X 'main.Commit={{.Commit}}'"

# Artifacts
# Control exactly what gets synced via rsync.
artifacts:
  include:
    - "migrations/"
    - "files/"
    - "Dockerfile.vps"
  exclude:
    - "data/"       # Never sync the local DB folder
    - "*.db"
    - ".env"        # .env is handled separately via 'sync_env_file'

# ==============================================================================
# ENVIRONMENTS
# ==============================================================================
environments:
  # ----------------------------------------------------------------------------
  # PRODUCTION
  # ----------------------------------------------------------------------------
  prod:
    # SSH Connection Details
    host: "vps.example.com"
    user: "deploy_user"
    ssh_port: 22
    ssh_key: "~/.ssh/id_ed25519_prod" # Optional: Use specific key instead of agent
    
    # Paths
    target_dir: "/home/deploy_user/web/my-awesome-app"
    sync_env_file: ".env.prod" # Local file to be uploaded as '.env' on remote

    # Database Management (for 'deploy db push/pull')
    database:
      driver: "sqlite"
      source: "data/app.db" # Relative path to project root

    # Infrastructure (for 'deploy traefik')
    traefik:
      version: "latest" # Or "v3.0"
      email: "admin@example.com" # For Let's Encrypt
      cert_resolver: "myresolver"
      network_name: "traefik-net"
      dashboard: true
      # Basic Auth for Dashboard (user:hash). Generate via 'deploy gen-auth'
      dashboard_auth: "admin:$2y$05$..." 

    # Runtime Configuration (The Quadlet)
    quadlet:
      service_name: "my-awesome-app"
      description: "Production Service"
      image: "localhost/my-awesome-app:latest"
      dockerfile: "Dockerfile.vps" # The file used for 'podman build' on remote
      network: "traefik-net"
      auto_restart: true
      timezone: "Europe/Vienna"

      # --- Security (Distroless/Non-Root) ---
      # If using distroless/static, set these to 65532.
      # The tool will automatically run 'podman unshare chown' on 'chown_volumes'.
      container_uid: 65532
      container_gid: 65532
      chown_volumes: 
        - "./data" # Fix permissions on this host dir before starting

      # --- Resources & Health ---
      # memory: "512M"
      # cpu: "0.5"
      # health_cmd: "wget -q --spider http://localhost:8080/ || exit 1"

      volumes:
        - "./data:/data:Z"
        - "./migrations:/migrations:ro,Z"
      
      # --- Traefik Router Abstraction ---
      # Generates all necessary Traefik labels automatically.
      router:
        host: "app.example.com"
        internal_port: 8080
        https_redirect: true
        # Advanced options:
        # path_prefix: "/api"
        # strip_prefix: true
        # basic_auth_users: ["user:hash"]
        # rate_limit:
        #   average: 100
        #   burst: 50
        # headers:
        #   X-Custom: "Value"
      
      # Standard Env Vars (in addition to the synced .env file)
      env_vars:
        - "APP_ENV=production"
        - "RUNNING_IN_CONTAINER=true"
```

---

## 🛠 CLI Reference

### 1. Deployment
Builds the app locally, syncs artifacts, builds the container remotely, updates Systemd, and verifies health.
```bash
deploy run prod
```
*   `--dry-run`: Print all commands (SSH, rsync, build) without executing them.
*   `-v`: Verbose output (see live stdout/stderr of remote commands).

### 2. Infrastructure Setup
Installs and configures Traefik with Let's Encrypt, Docker Socket proxying, and Dashboard.
```bash
deploy traefik prod
```

### 3. Observability
Stream logs directly to your terminal.
```bash
# Stream Systemd logs (includes startup/shutdown events + app stdout)
deploy logs prod

# Stream raw Container logs (via Podman driver)
deploy logs --podman prod
```

### 4. Database Operations
Manage your SQLite data. **Note:** `push` stops the remote service during transfer to prevent corruption.
```bash
# Download remote DB to local ./data/
deploy db pull prod

# Upload local ./data/ to remote (Dangerous!)
deploy db push test
```

### 5. Maintenance
Clean up disk space on the VPS by removing dangling images and build cache.
```bash
deploy prune prod
```

### 6. Utilities
```bash
# Generate a BCrypt hash for Basic Auth config
deploy gen-auth myuser mypassword

# Manually fix volume permissions on remote (if things get messed up)
# Target can be 'user' (reset to SSH user) or 'container' (set to container_uid)
deploy rights prod container
```

---

## 🧠 Concepts & Architecture

### The Deployment Pipeline
1.  **Validation:** Checks for local dependencies (`rsync`, `go`) and remote connectivity.
2.  **Build:** Cross-compiles the Go binary for Linux (`GOOS=linux`, `GOARCH=amd64/arm64`) locally. Injects version info via `LDFLAGS`.
3.  **Generate:** Creates a Quadlet (`.container`) file in memory based on `deploy.yaml`.
4.  **Backup:** Copies the existing binary on the server to `*.bak`.
5.  **Sync:** Rsyncs the binary, Dockerfile, migrations, and config to the VPS.
6.  **Activate:**
    *   Builds the container image on the VPS.
    *   Fixes volume permissions (if `container_uid` is set).
    *   Updates Systemd units.
    *   Restarts the service.
7.  **Health Check:** Polls `systemctl is-active`. If it fails, performs an **Automatic Rollback** to the backup binary.

### Rootless Permissions
When using **Distroless** images (which run as non-root user 65532), mounting host directories is tricky. The container cannot write to a folder owned by your SSH user (UID 1000).
*   **The Solution:** The tool uses `podman unshare chown -R 65532:65532 ./data` on the server. This maps the folder ownership into the container's user namespace, allowing it to write, while keeping it secure.

### Traefik Integration
The `router` config in YAML abstracts away the complex Traefik labels. It automatically handles:
*   `traefik.enable=true`
*   Host rules and Entrypoints.
*   Middleware chains (Auth -> StripPrefix -> Compress -> Headers).
*   Load Balancer ports.
