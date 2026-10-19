# Deploy Tool 🚀

**Deploy Go applications and existing registry images to a Linux VPS with rootless Podman and Quadlet.**

This tool replaces fragile Makefiles and complex Bash scripts with a single, declarative CLI. It uses **Podman**, **Quadlet**, and **systemd** for controlled service restarts, health checks, automatic rollback, and infrastructure management.

---

## ✨ Key Features

*   **Declarative Infrastructure:** Define your environments (Prod, Test), build settings, and runtime configuration in one `deploy.yaml`.
*   **Quadlet Native:** Automatically generates modern Systemd unit files for Podman, ensuring your containers start at boot and restart on failure.
*   **Safety First:**
    *   **Image Transactions:** Stages and validates images before restarting a single service; this is a controlled restart with a brief interruption.
    *   **Auto-Rollback:** Both modes restore the exact previous local image, Quadlet, managed files, environment file and original service activity.
    *   **Dry Run:** Preview the release plan without builds, SSH, pulls, synchronization, Git publication, or candidate files.
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
      # rollback: "manual" # Default: stop on changed DB after failed startup.
      # rollback: "restore" # Opt in to restoring the pre-release DB; can discard new writes.

    # Infrastructure is configured in server.yaml using deploy server init/provision.

    # Maintenance Page Configuration (Optional)
    # If enabled, a lightweight Nginx container runs in "standby" (Priority 1).
    # When the main app (Priority 100) stops, Traefik fails over to this page instantly.
    # To enable: deploy maintenance enable prod
    # To disable: deploy maintenance disable prod
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

## Registry image projects

`deploy init` still generates a Go binary project. Use `deploy init --mode=image`
for a registry image project; existing `deploy.yaml` files are never overwritten.
An image project needs Git, SSH and rsync locally, and needs no Go source, `go.mod`,
binary, Dockerfile, local Podman, or Go toolchain.

```yaml
app_name: internal-service
deployment:
  mode: image
# Optional: only these files are synchronized; there are no default artifacts.
# artifacts:
#   include: [config]
#   exclude: ["*.bak"]
environments:
  prod:
    host: vps.example.com
    user: deploy_user
    target_dir: /home/deploy_user/web/internal-service
    quadlet:
      service_name: internal-service
      image: docker.io/library/nginx:1.28-alpine  # Pin a verified digest in production.
      network: traefik-net.network
      network_aliases: [internal-service-prod]
      router:
        enabled: false
      env_file: ""
      auto_restart: true
      cpu: "0.5"
      memory: 256M
      health_cmd: "wget -q --spider http://127.0.0.1/"
      health_timeout: 60s
```

Use the existing commands:

```sh
deploy --dry-run -v release prod
deploy release prod
deploy release v1.2.3 prod
deploy status prod
deploy logs --podman prod
deploy restart prod
deploy prune prod
```

The project Git tag identifies the configuration release. `quadlet.image` identifies
upstream software. Image references require an explicit registry and tag or digest;
short names and credentials in references are rejected. Production configurations
should specify an exact version and `@sha256:...`. Registry authentication uses the
remote SSH user's existing `podman login` configuration. Every release pulls the
reference and pins the ID returned by that pull into `Image=sha256:...` with
`Pull=never`. Mutable tags cannot change confirmed deployments or rollback images.

`deployment.mode` defaults to `binary`. Image mode rejects `binary_name`, build
options and `quadlet.dockerfile`. An omitted `exec` preserves image startup defaults.
Mounted data ownership is managed separately; image mode rejects `chown_volumes`.

### Remote prerequisites

Both release modes require Bash, Podman, a running user systemd manager, rsync,
flock, jq, GNU timeout and GNU coreutils including `sync -f`. Binary releases
still build locally and build their container image remotely. Their uploads are
staged before stopping the service. Rollback restores the saved image directly;
it does not rebuild it with the failed candidate's Dockerfile.

The compiled `build/<binary_name>` is always uploaded separately from artifacts;
`artifacts.exclude` applies only to the configured artifact files. Custom build
commands must write a nonempty regular file to `build/<binary_name>`. The previous
output is removed before each build; successful commands without output fail the
release. Artifact paths are preserved, including explicitly selected nested files.
Runtime images are built from the private staged artifacts, never the live target
directory. Include the Dockerfile and every required build input explicitly;
remote database files, backups, `.env` and transaction state are not build inputs.

Image deployments require Linux, **rootless Podman >= 5.4.2**, **systemd >= 257**, cgroup
v2, delegated CPU/memory controllers when limits are configured, a running user
systemd manager, rsync, flock, jq, GNU timeout, and the usual GNU
core utilities. Host URL checks additionally require curl. The Quadlet generator
must be installed at `/usr/lib/systemd/system-generators/podman-system-generator`
or `/usr/libexec/podman/quadlet`. Preflight reports missing requirements and never
installs packages. User lingering is required if services must run after logout.

The renderer uses `NetworkAlias`, `Pull`, and `HealthCmd` in `[Container]`, and
`Restart`, `RestartSec`, `CPUQuota` and `MemoryMax` in `[Service]`, following the
[Podman 5.4 Quadlet documentation](https://docs.podman.io/en/v5.4.2/markdown/podman-systemd.unit.5.html).
The candidate is checked by the actual remote generator in an isolated unit
directory before activation. This does not start services or create networks.
The live integration suite passed on Podman 5.4.2 and systemd 257.5 in an isolated
rootless test account on 2026-10-17. These versions define the supported floor.
The local generator check also passed on Podman 5.7.0/systemd 259.

Existing networks are reused. A `*.network` reference must resolve to an existing
network Quadlet. Plain names refer to existing Podman networks. This feature does
not provision networks. The container remains `systemd-<service_name>`; give each
environment distinct service names, target directories and network aliases.

### Routing, environment files and health

| Setting | Behavior |
| --- | --- |
| `router.enabled` omitted | Infer routing from `domain`, `host` or `rule`, as before. |
| `router.enabled: true` | Require one of those routing targets. |
| `router.enabled: false` | Generate `traefik.enable=false`, no router labels or Traefik service dependency. |
| `quadlet.env_file` omitted, binary mode | Use `<target_dir>/.env`. |
| `quadlet.env_file` omitted, image mode | No file unless `sync_env_file` is configured. |
| `quadlet.env_file: ""` | No file; incompatible with `sync_env_file`. |
| Nonempty `quadlet.env_file` | Absolute remote path, or resolve relative to `target_dir`. |

`sync_env_file` writes only `<target_dir>/.env`. A different `env_file` together with
synchronization is rejected. Without synchronization, a referenced remote file
must already exist and be readable; no empty replacement file is created.
Environment contents are never placed in manifests or printed by dry runs.

`health_cmd` runs **inside the container** through `podman healthcheck run`, with
periodic Podman checks remaining enabled. `health_url` runs curl **on the VPS host**;
it is never reinterpreted as a container URL. Both must pass when configured.
`health_timeout` bounds the entire readiness loop, including individual probes:
60 seconds by default for images, 30 seconds for binaries. A positive Go duration
up to 24 hours is accepted. Without application checks, the result explicitly says
that only service activity and image identity were verified.

Custom labels are preserved and merged with generated labels; conflicting values
for generated keys are configuration errors. `auto_restart: true` emits
`Restart=on-failure` and `RestartSec=5s`. CPU values are counts (`0.5` → `50%`, `1` →
`100%`) or explicit percentages. Memory is a systemd `MemoryMax` limit. Binary
suffixes such as `512MiB` and `512MB` normalize to `512M`; overflowing values
are rejected before any remote action.
These corrections also apply to binary projects: **an explicit `router.enabled:
false` is now respected**, restart rules now take effect, and CPU limits use the
correct section. Review existing configurations for these intentional changes.

### Transaction state and rollback

State is private to the SSH user under `<target_dir>/.deploy/image/` or
`<target_dir>/.deploy/binary/`:

```text
current.json                 # Atomic {current, previous} release pointers
pending.json                 # Unfinished transaction
releases/<release_id>/
  manifest.json              # Git version/commit, reference, image ID, digests, checks
  candidate.container        # Exact pinned Quadlet
  before/                    # Previous files, commit record and service activity
staging/<release_id>/         # Uploaded candidates
```

Locks under `~/.local/state/deploy/locks/` are keyed by both service and target
directory. Releases, database replacements and service actions share these locks.
A target directory belongs to one service and deployment mode; another service
cannot overwrite its state. The next operation resolves pending recovery first.
After an interrupted commit, matching committed pointers, Quadlet and readiness
allow recovery to finish the commit; otherwise it restores the prior snapshot.
Backups, pending records, installed files, image storage, unit replacements and commits are
flushed with GNU `sync -f` before proceeding. This flushes file contents and
directory metadata, including when units and project files use different
filesystems. Storage must honor the operating system's durability requests.

Pull and validation failures leave the running service untouched. Activation
failures restore the byte-for-byte old Quadlet, exact retained image, overwritten
artifacts, synchronized .env, old commit record and original active/inactive state.
A failed first installation removes its new managed files and Quadlet. If rollback
fails, both failures are reported and pending state/backups remain for recovery.
File changes are tracked individually: unrelated files and mounted application
data remain. Symlink artifacts, unsafe paths and `.deploy` overlaps are rejected;
artifact directories are merged without deleting unrelated contents. Files managed
by the previous release that are absent from the new artifact set are backed up
and removed; rollback restores them. Externally managed configuration changes
remain outside rollback.
Directory modes are applied after their contents are installed, allowing
read-only configuration trees to deploy and roll back. Binary artifact uploads
also merge directories and honor `artifacts.exclude`; unmanaged files remain.
Binary volume ownership changes remain outside file rollback, and skip the
configured database's backup directory.

Existing unmanaged Quadlets (including binary deployments) cannot be adopted
implicitly. Use a new service name or carry out a separately reviewed migration.
Image service actions reject a different `target_dir` for an already managed service,
so changing directories cannot bypass recovery. `enable` and `disable` persist
`WantedBy` through a managed Quadlet override, since generated systemd units cannot
be enabled with `systemctl enable`. The override survives releases; interrupted
changes are recovered using `boot-pending.json` before another mutation.

Current, previous and unfinished images have local retention tags and survive
`deploy prune` (`podman image prune -f`). Old releases and tags are retained;
there is no automatic garbage collector in this version.

Database pull downloads and validates a complete SQLite snapshot before replacing
the local database, and backs up an existing local database with SQLite's backup
API. Stop the local application first. Replacement uses SQLite's transactional
restore API, leaving WAL/journal handling and crash recovery to SQLite instead of
moving sidecars. Database push stages a local snapshot,
requires a confirmed inactive remote service, and creates a consistent backup
including committed WAL data before replacement. Backup or transfer failures
abort the update. Interrupted replacements recover under the same deployment
locks before the next release, database push or service action. Both database
commands return before file writes or external commands when `--dry-run` is set.

Before replacing a database, a verified SQLite snapshot is retained in `backup/`
next to that database. Files include the database stem, the environment, the
operation, a UTC timestamp and a unique ID, for example:

```text
data/app.db
data/backup/app.prod.before-push.20261019T193000123456789Z.database-...db
data/backup/app.local.before-pull-from-prod.20261019T193100-...db
```

Pull protects the existing **local** database; push protects the existing **remote**
database. The backup directory is mode `0700`, snapshots are `0600`, and failures
abort the operation. Pull and push still update a latest `.bak` for compatibility, but unique
backups are never overwritten or automatically deleted. Explicit history cleanup is available when a retention policy is configured. Copy them off the server
for protection against storage failure.

When `database.driver: sqlite` and `database.source` are configured, both release
modes stop the service and retain a snapshot before candidate startup, even when
`stop_on_deploy` is false. A remote SQLite CLI supporting `.dump --preserve-rowids`
is required and checked during preflight. Failed startup rolls
back normally if the database is unchanged. If it changed, the default
`database.rollback: manual` keeps the service stopped, the data intact and the
transaction pending, with the pre-release snapshot path reported in the error.
This avoids running the old application against a changed schema or silently
discarding writes accepted by the candidate.

For manual recovery, keep the service stopped, preserve the changed database,
remove its sidecars and atomically replace it with the reported pre-release
snapshot, preserving its original ownership and mode; then rerun a service action
to complete pending recovery. Alternatively, set `database.rollback: restore`
**before releasing** to authorize automatic database restoration. That policy also
retains the candidate database as a `before-rollback` backup before rewinding it.
It can discard candidate writes, so use it only when that tradeoff is acceptable.

`server provision` checks every upload and service operation and reports success
only after Traefik is active. Requests for the unimplemented global auth and
Watchtower providers fail before provisioning starts. Configure authentication
through existing middleware or application router settings.

See [the Gotenberg example](examples/gotenberg/) for a private PDF service with no
published host port. A separate infrastructure repository, actual VPS deployment,
and PAM application integration follow outside this tool change.

### Verification

```sh
make test
```

The normal suite executes the transaction shell script with fake Podman/systemd
commands and real temporary files, including file restoration, failed rollback,
read-only directories, write/flush failures, target lock contention, and
interruptions around the commit. SQLite tests use the local `sqlite3` CLI; a
Python fixture verifies recovery of committed WAL data. It also checks generated
units against the locally installed Quadlet generator when available.

For live integration, use an **isolated disposable Linux VM or test account** with
rootless Podman and a user systemd manager, never a production account. Set a
verified Gotenberg Chromium image with digest:

```sh
DEPLOY_IMAGE_INTEGRATION=isolated \
DEPLOY_TEST_GOTENBERG_IMAGE='docker.io/gotenberg/gotenberg:VERSION-chromium@sha256:DIGEST' \
make test-image-deploy-integration
```

This explicit opt-in target never reads a user's `deploy.yaml`, uses a local runner unless invoked through the disposable harness,
and operates only on unique `deploy-it-*` services, network and temporary project
directories. It tests a healthy HTTP image, failed rollout with rollback, image
retention after prune, private Gotenberg DNS/health/restart, resource limits and
HTML-to-PDF from a second container. It also deploys a real Go executable, verifies
rollback of its image, read-only configuration and synchronized environment, and
backs up/replaces SQLite data owned by a mapped container UID in a private
directory. It pulls test images and reloads that test
account's user manager. The suite was run on Podman 5.4.2/systemd 257.5; also run it against
the versions installed on your target VPS before promoting a new runtime version. The tag-only HTTP and curl test clients
are disposable fixtures; the Gotenberg service is explicitly pinned.

To create that isolated environment automatically on an amd64 Linux development
machine with rootless Podman, subordinate UID/GID ranges and Go installed:

```sh
make test-image-deploy-isolated
```

The harness generates test SSH keys and known-host entries inside its disposable container and exercises real SSH and rsync. It uses a separate temporary Podman store and a disposable container
with its own systemd, test user and nested rootless Podman. It installs test
prerequisites **inside that disposable container** and removes the container and
its store afterward. The outer container is privileged within the invoking
user's rootless namespace; this is required for nested namespaces and systemd.
The harness forces local Podman (`--remote=false`). No project `deploy.yaml`,
existing container store or VPS is used. Base and
Gotenberg fixtures are pinned by digest; override `DEPLOY_TEST_BASE_IMAGE` or
`DEPLOY_TEST_GOTENBERG_IMAGE` to test another verified native image.

The completed live run on 2026-10-17 used Podman 5.4.2, systemd 257.5 and
Gotenberg Chromium 8.38.1 (`sha256:63545039126042c5df622675ccb3dd9efd8b5c1faa0ec5265b787737e6d6757e`
for linux/amd64). It passed first installation, an HTTP image update, failed
readiness with rollback, current/previous image retention after prune, boot
enable/disable, private DNS, health checks, restart, CPU/memory limits, and
HTML-to-PDF without a published Gotenberg port. Binary first installation with
`stop_on_deploy: true`, complete binary configuration rollback, a normalized
`128MiB` limit and SQLite backup/replacement under container UID 65532 also passed.


### Configuration validation and migration

Run `deploy validate prod` before upgrading a project, and `deploy validate --server`
for infrastructure configuration. Validation is local and runs without Git publication,
SSH or a build. Both YAML files reject unknown settings, duplicate keys, empty input
and extra documents. Supported aliases such as `router.host` continue to work.

The obsolete `environments.<name>.traefik` block was previously ignored. Remove it
from `deploy.yaml`: infrastructure version, email and network settings belong under
`server.yaml`'s `stack.traefik`; application network and routing settings belong under
`quadlet.network` and `quadlet.router`. Preserve the application's configured
`quadlet.router.cert_resolver` when migrating. Unused old options have no automatic
replacement. The validator reports this migration explicitly.

SSH commands use batch authentication and strict host-key verification. Establish
and verify the host key and load your private key into an SSH agent before deploying.
`ssh_known_hosts` can select a dedicated local known-hosts file for an environment.
A remote Git tag must match the local tag object and commit, including annotated
tags; deployment refuses a different remote target and never force-pushes tags.
Release names start with a letter or digit and use letters, digits, dot, underscore,
slash, plus or hyphen, so metadata substitution cannot introduce shell operators.

### Stable readiness and execution budgets

Readiness samples service activity, pinned image identity, container identity,
restart count and every configured application check. A failed check or a container
restart resets stabilization. The following settings are available under `quadlet`:

```yaml
health_required: true
health_cmd: "wget -q -O /dev/null http://127.0.0.1:8080/health"
health_timeout: 60s
health_stable_for: 5s
health_interval: 1s
health_successes: 3
```

Use an endpoint implemented by your application that checks its dependencies,
including its database. New generated production templates require an application
check and include an endpoint to adapt. Existing configurations default
`health_required` to false and report when only service/image checks are available.
Stabilization defaults to five seconds, one-second samples and three consecutive
successes. The total timeout must accommodate the stabilization settings.

Each environment accepts operation budgets:

```yaml
timeouts:
  connect: 10s
  transfer: 10m
  build: 15m
  pull: 15m
  database: 5m
  service: 60s
  recovery: 5m
  shutdown_grace: 10s
```

These are the defaults. Larger builds and databases need suitable overrides. Remote
phases retain deployment locks after SSH loss until the worker and any recovery
finish. Recovery has an independent watchdog; an expired recovery leaves its pending
state intact. Finite local subprocesses terminate as process groups. Streaming logs
remain open until interrupted.

Binary images are built and their Quadlets validated before the application stops.
Each release captures local inputs in a private directory under a project lock;
subsequent uploads use that captured payload and verify its checksums. Database
pulls serialize backup and replacement by destination path, including across source
environments. Stop local application writers before an intentional local replacement.
`rights` requires an inactive service, shares deployment locks and preserves the
private database backup directory. A failed ownership or prune step exits nonzero.

Successful releases and status report release, commit and image identity. New managed
units also contain an activation gate: after a reboot or process death, unresolved
pending state blocks automatic startup until `deploy start <env>` performs recovery
under the original configuration. A committed candidate can resume verification;
uncommitted candidates follow the configured database rollback policy.

### Capacity and explicit history cleanup

Environments accept `storage.min_free_bytes` (default 67108864),
`storage.min_free_inodes` (1024), and `storage.image_reserve_bytes` (268435456).
Before stopping a service or replacing data, preflight checks space for staged files,
database/WAL snapshots, restoration and the reserve. Image expansion and unrelated
storage use can still exhaust a filesystem after preflight; write failures preserve
recovery state. Local database pulls check capacity before replacing local data.

Retention is disabled by default. To enable it explicitly:

```yaml
database:
  driver: sqlite
  source: data/app.db
  rollback: manual
  retention:
    keep_last: 10
    max_age: 720h
storage:
  releases:
    keep_last: 5
    max_age: 720h
```

`keep_last` is a minimum recent count; when both options are set, an entry must also
be older than `max_age` to qualify for deletion. Preview exact remote candidates with
`deploy prune --history --list prod` or `deploy --dry-run prune --history prod`.
Apply the configured policy with `deploy prune --history prod`. Local pull snapshots
can be previewed with `deploy db prune-local --list prod` and cleaned with
`deploy db prune-local prod`; this uses the same policy and local destination lock.
Off-host bundles are retained separately and are not deleted by these commands.
Preview reads remote
inventory under coordination locks and creates only temporary inventory/lock metadata.
The release policy also controls completed database-operation archives. Cleanup
protects current and previous releases and all retained history backup references; pending
recovery or corrupt inventory stops cleanup. Older snapshots without managed metadata
and unrelated files are retained. Ordinary `deploy prune prod` continues to prune
unused image/cache data under the shared Podman storage lock.

Database push recovery retains the published incoming database in a separate
`before-push-rollback` snapshot before rewinding it. Recovery markers keep the same
candidate snapshot across interruptions. For applications that must not access
backups through their writable data mount, use a nonzero `container_uid`; container
UID zero maps to the SSH user in a rootless deployment.

### Off-host database snapshots and restore drills

Configure a destination on a separate machine explicitly:

```yaml
database:
  driver: sqlite
  source: data/app.db
  off_host:
    host: backup.example.com
    user: backup
    ssh_port: 22
    ssh_key: /home/sal/.ssh/backup
    target_dir: /home/backup/foodlog
    # Point your application check at the disposable database from this variable.
    restore_check: './scripts/check-restored-database "$DEPLOY_RESTORE_DATABASE"'
```

`deploy db backup prod` downloads a consistent source snapshot under deployment
locks, validates SQLite integrity, transfers it to the configured host, verifies its
checksum and SQLite integrity again, and publishes an immutable bundle containing
`database.db` and `receipt.json`. A receipt is recorded on the source only after the
remote bundle is confirmed. Interrupted incoming directories are not completed
backups. The destination requires SSH, rsync, SQLite and the standard GNU file tools;
configure its storage access controls and verify its SSH host key.

The command prints a backup ID. Run `deploy db restore-check prod <id>` to retrieve
that bundle into a private disposable directory, verify its checksum and SQLite
integrity, then execute `restore_check` with `DEPLOY_RESTORE_DATABASE` pointing to the
retrieved database. The check must exercise an application read. Command output is
suppressed to avoid logging database records. An off-host copy is useful recovery
protection only after its restore procedure has been tested.

### Release verification

`make release-check` requires the normal/race/vet suite, real SSH/rootless Podman
integration and the VM interruption matrix. CI runs local checks on Linux and macOS,
real transport against the pinned runtime floor and a freshly resolved current
runtime digest, and VM recovery before the `release-gate` job succeeds. Configure
repository branch/release protections to require that job before publishing.

`make test-deploy-vm-crash` builds a disposable bootable VM from the pinned integration
base, using a temporary disk and generated credentials. It power-cuts and reboots
both deployment modes after pending publication, database snapshot, candidate
activation, database-rewind journaling, commit and pending removal, then verifies
files, database contents, application responses and deployment identity. QEMU uses
KVM when available and otherwise uses software emulation. This test requires the
QEMU tools and enough temporary storage; it never boots or modifies a user's VM or
accesses production configuration.
