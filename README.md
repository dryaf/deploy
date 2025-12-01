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
    *   **Maintenance Mode:** Automatic "Standby" container that serves a nice HTML page whenever your main app is stopped or restarting.
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
  # Available variables:
  #   {{.Version}}      -> v1.2.3 (Full Git Tag)
  #   {{.MainVersion}}  -> v1.2   (Major.Minor)
  #   {{.Commit}}       -> 8f3a1...
  #   {{.Date}}         -> 2024-01-01T12:00:00Z
  #   {{.GoVersion}}    -> go1.25.3
  ldflags: "-s -w -X 'main.Version={{.Version}}' -X 'main.Commit={{.Commit}}'"

  # Optional: Custom Build Command
  # If defined, 'cmd' overrides the standard 'go build' logic.
  # Useful for building inside Docker/Podman (CGO/SQLite support).
  # cmd: >-
  #   podman run --rm -v "$(pwd):/app" -w /app golang:alpine
  #   go build -ldflags="-X main.ver={{.Version}}" -o build/server .

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

    # Maintenance Page Configuration (Optional)
    # If enabled, a lightweight Nginx container runs in "standby" (Priority 1).
    # When the main app (Priority 100) stops, Traefik fails over to this page instantly.
    maintenance:
      enabled: true
      title: "Under Maintenance"
      text: "We are currently upgrading the system. Please try again in a minute."

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