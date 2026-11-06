# Deploy Tool 🚀

Implementation status and remaining acceptance gates are tracked in
[IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md), with the original acceptance
criteria mapped in [ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md). The initialized Telegram
backup profile supports [reviewed held-outcome release and canonical legacy
rollback](examples/notification-worker/BACKUP_CUTOVER.md). Broader recovered/Signal,
command/LED activation and external qualification remain incomplete.

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
      network: "traefik-net.network" # Reference the network Quadlet created by server provision
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

Both release modes reuse existing networks. `deploy server provision` creates
`<network_name>.network` using `server.yaml`'s `stack.traefik.network_name`
(default: `traefik-net`). Set `quadlet.network: traefik-net.network` for that
default, or `<network_name>.network` for a custom name. Provision Traefik and deploy
the application with the same remote SSH user so they share the rootless network.

A `*.network` reference must resolve to an existing network Quadlet. Plain names
refer to existing Podman networks, so `traefik-net` is not equivalent to
`traefik-net.network`. Application releases do not provision networks. The container
remains `systemd-<service_name>`; give each environment distinct service names,
target directories and network aliases.

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

### Named servers and backup declarations

`server init` now creates a version 2 inventory. Existing single-server files and
inline application SSH settings remain supported. A v2 inventory can contain both
application hosts and hosts with no infrastructure stack:

```yaml
version: 2
servers:
  vps-main:
    host: vps.example.com
    user: deploy
    ssh_key: ~/.ssh/vps-admin
    ssh_known_hosts: ~/.ssh/known_hosts
```

An application environment can use `server: vps-main` in place of `host`, `user`,
`ssh_port`, `ssh_key`, and `ssh_known_hosts`. Mixing the two connection forms is an
error, including explicitly empty inline fields. References are resolved for all
application commands before remote work. Target directories, database settings,
and runtime settings stay in `deploy.yaml`. Colliding services, target directories,
database bindings, and shared writable volume sources on the same visible SSH
account are rejected. DNS aliases cannot establish that two names are the same
physical account; remote ownership checks remain necessary.

The default inventory is `server.yaml` beside the application file; server-only
commands use `./server.yaml`. `deploy --servers /path/server.yaml …` overrides that
path. There is no parent-directory search. Relative SSH paths in v2 are relative
to the inventory file, and `~` means the operator's home. Worker storage and secret
paths stay absolute paths on the worker. V2 entries require an explicit user.

```sh
deploy --servers ./server.yaml validate --server
deploy server provision vps-main
# Prints a candidate without modifying the legacy input:
deploy --servers ./legacy-server.yaml server migrate vps-main > server-v2.yaml
deploy --servers ./server-v2.yaml validate --server
```

Keep a migrated inventory beside the original when retaining relative SSH paths.
V2 provisioning always requires a name, even for one entry, and only provisions
an explicitly configured Traefik stack. The original `deploy server provision`
syntax still accepts a legacy file.

[The shared prod/test example](examples/backup-worker/deploy.yaml) and
[worker policy example](examples/backup-worker/server.yaml) demonstrate distinct
services and data directories. `database.backup.enabled` defaults to false and
requires a SQLite database in a dedicated host directory bind when enabled.
Raw Podman overrides, overlapping mount destinations, and unsupported mount
options are refused for this profile. The declaration does not change existing
release-time database protection or `deploy db backup`.

**Implementation status:** the Linux worker, restricted source helper, archive
list/pull/recovery, scheduling, lifecycle commands, notifications, retention, and
isolated restore-check contracts are implemented. Setup stages an installation
with timers disabled. Actual ARM64/rootless execution, disk crash durability,
Signal/Telegram receipt, and external missed-run routing still require live
qualification before activation. No host was provisioned or image published by
this implementation. See the [worker runbook](examples/backup-worker/README.md)
and [qualification plan](BACKUP_WORKER_PLAN.md). Keep existing backup schedules
until the rollout acceptance window passes.

Worker policy validation currently supports `hourly`, `daily`, and `weekly`, with
an explicit freshness value large enough to cover the interval and allowed run
delay. Other calendar expressions are rejected until occurrence-policy handling
is implemented. Defaults are hourly, Europe/Vienna, freshness 2h, run deadline
40m, and 10 GiB per snapshot. Retention describes **all snapshots within 720 hours
and at least the newest 48**, not a maximum of 48 snapshots. Validation does not
read remote secrets, prove monitor identities, link accounts, or send messages.

Automatic retention needs a new, fresh successful capture for each instance;
failed or missing sources keep their existing history. Each capture can authorize
only one automatic pass, recorded in the archive before deletion. Interrupted
passes await another successful capture or explicit scoped operator pruning.
Independent health checks include the maintenance timer and service.

The [shared notification worker plan](NOTIFICATION_WORKER_PLAN.md) describes the
proposed extraction of Signal/Telegram delivery, pi-status-led support, and future
vulnerability/uptime producer integration, including migration and qualification.
The [interactive command plan](INTERACTIVE_WORKER_PLAN.md) tracks the partial
implementation of durable worker commands, scoped permissions, confirmation,
native/opaque notification references, gateway discovery and paginated queries,
and staged Telegram/Signal intake. Backup has read-only command staging and locally
tested reminder handlers. Its private shared exporter now preserves original reply
context, maps batch/daily/catch-up obligations, persists cancellation intent, and
replays pinned submissions and receipt acknowledgements through the local gateway.
Large histories use bounded pages; only the final catch-up acceptance settles the
outage. A private delivery-monitor publisher verifies eligible coverage and retains
recovery intent across restart and uncertain heartbeat responses.
The private backup evaluator now builds reports from current catalogue, verification,
run and host-probe evidence, committing incident changes and exports together.
Healthy status expires with its supporting evidence.
The gated report-service entry point connects evaluation, delivery and independent
monitoring. A managed Telegram-only cutover now installs and activates these native units;
see [backup cutover](examples/notification-worker/BACKUP_CUTOVER.md) for exact
review, apply/resume and fenced revocation commands. Compatible legacy senders honor the local
ownership fence, and compatible setup/recovery refuses to restore legacy delivery
while that fence is present.
The legacy outbox notifier now reserves every provider attempt durably before
sending and commits the outcome together with outbox acceptance. A reservation
left by a crash is recorded as ambiguous before any later attempt; the existing
legacy retry/fallback policy still applies and can produce duplicates after an
uncertain outcome. Attempted events are not automatically folded into fresh
batches or exported as proven-unsent shared work. Inspect the retained evidence
with `deploy backup-worker delivery-review --local --state-dir /absolute/state`.
This command does not settle reservations, send messages or authorize cutover.
Older helper history is explicitly incomplete. The journal retains exact bounded
envelopes and has an 8,192-record limit (two records per attempt); exhaustion or
lost/corrupt journal/lease evidence stops legacy sending and requires offline
reconciliation. It has no automatic pruning or reset. Manual provider test sends
are outside this outbox journal. Managed cutover preserves accepted history, exports explicitly reviewed unsent
work, and holds ambiguous/incomplete outcomes without resending. Releasing held
outcomes and restoring legacy delivery after shared revocation remain unfinished.
The shared worker now journals delivery ownership outside SQLite and resumes exact
interrupted transitions. Missing or conflicting ownership history fails closed;
older ownership records migrate as explicitly incomplete historical evidence.
Offline snapshots can now be captured and verified:

```sh
deploy notification-worker snapshot --local --config /absolute/runtime.json --state-dir /absolute/state --to /absolute/new-snapshot
deploy notification-worker snapshot-verify --local --config /absolute/runtime.json --from /absolute/new-snapshot
```

Capture requires the worker to be stopped and creates a new private directory;
it never overwrites a snapshot. The artifact is bound to the worker identity and
configuration. To start local offline recovery, use the exact `id` and
`database_hash` from the verified snapshot manifest:

```sh
deploy notification-worker recovery-prepare --local \
  --config /absolute/runtime.json --state-dir /absolute/state \
  --from /absolute/new-snapshot --snapshot-id n-REPLACE_WITH_VERIFIED_SNAPSHOT_ID \
  --database-hash REPLACE_WITH_VERIFIED_DATABASE_HASH
```

This rotates the recovery epoch and quarantines restored messages and commands
in a separate candidate. Preparation also checkpoints the original database's
committed WAL, then seals a private `recovery/<operation>/original.db` copy with
an independent manifest and database acknowledgment. It returns the recovery
`operation` ID needed by producer reconciliation and `original_source_hash`, the
digest of that preservation record. Retain the recovery directory and canonical
original `state.db`; both remain checked while publication is unfinished. LED
recovery reads the sealed copy and refuses incomplete preservation, changed source
data, archive corruption or unaccounted WAL contents. Keep the original snapshot:
the same command resumes interruption or lost output only when its full verified
manifest matches the prepared operation. It refuses a running worker, missing
state, changed snapshot or configuration. Global `--dry-run` checks arguments and
configuration without opening either state or snapshot. Preparation never starts
services, sends messages or publishes the candidate. Final recovered-state
publication and managed lifecycle recovery remain unfinished; ordinary startup
will continue to refuse this quarantined installation.

Review the durable recovery stages without advancing them:

```sh
deploy notification-worker recovery-status --local \
  --config /absolute/runtime.json --state-dir /absolute/state \
  --operation n-REPLACE_WITH_RECOVERY_OPERATION
```

This requires the existing reconciliation projection created by
`recovery-prepare`. It reports completed and pending producer, command, intake,
message-disposition and LED stages. `retained_fenced` identifies preserved
uncertainty, including unreconciled producers and inherited LED faults. Invalid
or missing committed evidence causes an error. The review acquires the worker
and producer leases together, opens the projection read-only, and hashes committed
logical database contents (including WAL-visible rows) and independent evidence.
`review_hash` identifies that observation; it is not publication or activation
permission. Intake still needs fresh provider cutoffs and stopped-receiver checks
at activation. The command does not initialize missing state, complete intents,
acknowledge receipts, execute commands or contact providers/LEDs.

Capture an immutable copy of that exact reviewed projection with:

```bash
deploy notification-worker recovery-checkpoint --local \
  --config /absolute/private/runtime.json --state-dir /absolute/private/state \
  --operation n-... --review-hash EXACT_REVIEW_HASH
```

The checkpoint is stored under `recovery/<operation>/checkpoints/<review_hash>/`.
Its intent retains the review, operation and candidate binding; its seal and
completion record pin the database bytes and logical projection. The copy includes
committed WAL data and preserves implicit row IDs. The worker and all producer
leases remain held through capture and repeated evidence validation. Exact retry
resumes interrupted work; a stale review or damaged sealed copy is refused.
Checkpointing does not change the reviewed source or enable delivery/execution.

The checkpoint also retains byte-for-byte copies of the recovery-stage JSON,
canonical identity/recovery records, complete enrolled ownership histories and
administrative intent/result files. `evidence-intent.json`, `evidence-sealed.json`
and `evidence.json` bind the file inventory to the checkpoint; numbered files in
`evidence/` hold the original bytes. A completed archive with missing or damaged
files is refused. An internal reader can validate these retained proofs after
canonical ownership heads change, without trusting the new live files.

Recovery review now verifies archived administrative records against the retained
original database and independent intents/results. Unfinished intents remain
fenced; missing result acknowledgments remain pending and are not recreated.
The stronger evidence fingerprint includes full ownership and administrative
history. Obtain a fresh `recovery-status` review after upgrading from the earlier
projection-only implementation; its older review hashes will be stale.

This is a publication prerequisite, not a complete recovery archive or an
activation grant. Retain the original/source/candidate databases, reconciliation
projection and all independent stage/ownership histories together with the
checkpoint. A later publication coordinator must revalidate current evidence,
complete service/provider handoffs and preserve ancestor proofs. Checkpoints are
bounded to 128 per recovery operation and the configured database/free-space
limits; automatic deletion of retained checkpoints is not implemented.

Verify a retained historical checkpoint independently of the current canonical
state with its original configuration and exact digests:

```bash
deploy notification-worker recovery-checkpoint-verify --local \
  --config /absolute/private/runtime.json --state-dir /absolute/private/state \
  --operation n-... --review-hash EXACT_REVIEW_HASH \
  --checkpoint-hash EXACT_CHECKPOINT_HASH
```

This offline command holds the existing worker lease and reads the retained
source, candidate, original and checkpoint databases immutably. It validates
checksums, schemas, epochs, SQL acknowledgments, saved ownership history and the
sealed evidence archive. It rechecks inputs before returning a deterministic
`proof_hash`. It does not use canonical `state.db`, current ownership heads,
mutable stage JSON or `reconciled.db`, so a later canonical transition does not
invalidate an intact historical proof. Missing or altered retained inputs are
errors; verification creates or repairs nothing.

The proof records a historical checkpoint. It does not establish that publication
completed, that services are stopped, or that provider cutoffs are fresh. Delivery,
execution and publication flags remain false. Ancestor-chain integration and
completed publication authority still need implementation.

Prepare a database image for a later publication step using that exact proof:

```bash
deploy notification-worker recovery-publication-prepare --local \
  --config /absolute/private/runtime.json --state-dir /absolute/private/state \
  --operation n-... --review-hash EXACT_REVIEW_HASH \
  --checkpoint-hash EXACT_CHECKPOINT_HASH --proof-hash EXACT_PROOF_HASH
```

Preparation requires the current review and holds the worker and producer leases.
It copies the checkpoint to `publication.db` in the same checkpoint directory and
adds one SQLite metadata anchor containing the historical proof. It verifies that
all other logical data, schema, header values and implicit row IDs match the
reviewed checkpoint. `publication-intent.json`, `publication-sealed.json` and
`publication-image.json` bind the resulting bytes to the proof and `image_hash`.
Interrupted unsealed copies can be resumed; damaged or missing sealed images are
refused. An existing anchor requires future ancestor reconciliation and cannot be
overwritten. Keep these artifacts with the retained checkpoint and evidence.

This writes only a prepared image. Canonical `state.db`, current reconciliation,
source fences and pending stages remain unchanged; publication, delivery and
execution flags stay false. The separate `recovery-publish` step below installs
the canonical database; managed activation still needs current service/provider
checks and ancestor-chain handling. A changed recovery review requires a new
checkpoint and image. `--dry-run` validates arguments/configuration only.

Verify a completed prepared image using its original configuration and exact pins:

```bash
deploy notification-worker recovery-publication-verify --local \
  --config /absolute/private/runtime.json --state-dir /absolute/private/state \
  --operation n-... --review-hash EXACT_REVIEW_HASH \
  --checkpoint-hash EXACT_CHECKPOINT_HASH --proof-hash EXACT_PROOF_HASH \
  --image-hash EXACT_IMAGE_HASH
```

This read-only command validates the complete retained checkpoint proof, the
image's SQLite anchor and unchanged logical contents, exact database bytes, and
the preparation intent, seal and completion record. It holds the existing offline
worker lease and checks inputs and directory identities again before returning.
It does not read canonical `state.db`, current ownership/identity records or
mutable reconciliation files, so an intact historical image remains verifiable
after those files advance. Missing or damaged evidence is an error; verification
never resumes preparation or recreates its completion record. The output matches
the original prepared image, with all permission flags false. It proves completed
preparation, not a completed canonical database switch or current activation
readiness. Keep the entire retained proof set; `publication.db` alone is insufficient.

Install that exact image as canonical state, retaining recovery quarantine:

```bash
deploy notification-worker recovery-publish --local \
  --config /absolute/private/runtime.json --state-dir /absolute/private/state \
  --operation n-... --review-hash EXACT_REVIEW_HASH \
  --checkpoint-hash EXACT_CHECKPOINT_HASH --proof-hash EXACT_PROOF_HASH \
  --image-hash EXACT_IMAGE_HASH
```

Publication holds the offline worker and producer leases and rechecks the current
review, image and retained proofs. Pending durable preparation blocks publication.
An absent producer may remain explicitly fenced with delivery/command reconciliation
pending; its unfinished message decisions still block the swap. Provider activation
rechecks remain separate. LED adoption can be skipped only without configured
bindings and without retained LED history or recovery evidence.

`recovery-publication.json` freezes the reviewed reconciliation. The publisher
copies the image, seals it in `recovery-publication-sealed.json`, then atomically
exchanges it with `state.db`. The old canonical database remains at
`recovery-publication-exchange.db`, in addition to the retained original archive.
`recovery-publication-complete.json` acknowledges the synced exchange. Both sides
are verified against exact hashes on resume; damaged sealed files are never
rebuilt. Only a checkpointed original's empty WAL/journal and rebuildable SHM index
may be removed under the frozen intent. Nonempty WAL/journal data blocks publication.

After freezing, other reconciliation commands are refused. Retry **the exact same
`recovery-publish` command** to finish an interruption or obtain the completed
result before adopting a verification runtime. A lost final acknowledgment can be completed from the intact sealed pair;
an inconsistent completion or rolled-back completed pair is an error. Keep all
publication and historical artifacts together. Independent checkpoint/image
verification remains available after the swap.

`canonical_installed: true` reports the completed database transition. Normal
startup remains blocked, and delivery/execution flags remain false. Managed
activation, current service/provider handoffs and subsequent recovery epochs are
still unfinished; publication does not start a service or enable notifications.

An existing managed verification installation can now adopt the published identity:

```bash
deploy notification-worker adopt-recovery --local --spec /absolute/adoption-spec.json
```

Use the normal installation spec with one additional field,
`"recovery_commit_hash": "EXACT_COMMIT_HASH"`, taken from
`recovery-publish`'s `commit.commit_hash`. Keep the installation root, state,
configuration and exclusions unchanged, and supply the new executable and its
SHA256. Adoption requires the installation's old identity to match the publication's
original identity. It stops the pinned unit, takes the installation lease, verifies
the complete retained proof and published database, and promotes a recovery-capable
executable/helper before starting the new identity **in verification mode**.

The state retains `recovery-verification.json` and an independent
`recovery-verification-complete.json`. Before that completion, retries require the
exact published database bytes. Later verification-runtime writes are allowed;
restarts check both adoption records, the live SQL publication anchor, quarantine
binding, identity and retained historical evidence. Missing completion after runtime
writes is an error, not permission to rebuild authority. Normal `serve` remains
blocked. Status reports `verification_only`, `installation_revision` and the exact
`recovery_commit_hash`; delivery, command execution and provider intake remain off.

Use `resume --local --root /absolute/installation` for an interrupted adoption;
the original spec and input executable are no longer needed after the intent is
published. Subsequent `update` specs must omit `recovery_commit_hash`: the installer
preserves its accepted pin automatically and checks both executables' recovery
capability. Updates and uninstall remain supported. Retained executable rollback
cannot cross the recovery identity/helper transition. Repeated recovery and
schema/configuration migration of the adopted state still require ancestor support.
This operation does not qualify or activate production.

After gateway publication, install the corresponding Signal relay runtime into a
separate, already existing empty directory owned by the service user (mode 0700):

```bash
deploy notification-worker recovery-signal-install --local \
  --config /absolute/runtime.json --state-dir /absolute/gateway-state \
  --commit-hash EXACT_GATEWAY_PUBLICATION_COMMIT \
  --relay-source /absolute/retained-inbox \
  --relay-destination /absolute/recovered-inbox
```

The command obtains the exact cursor from the gateway's verified publication and
holds its worker lease throughout installation. It verifies the original relay
completion and candidate, retains the old inbox, and binds the source to exactly
one destination directory. Both directories and their lock files must remain in
place. The first source intent fences ordinary source startup; the destination is
also fenced throughout installation. The new database preserves historical events
and reserved sequence gaps, with a matching runtime baseline in SQLite and files.
An immutable image seal and independent source/destination completion records
support exact retry of the same command after interruption. A sealed image is
never silently rebuilt. Keep both directories and all recovery artifacts.

This installs **verification state only**. It contacts no Signal provider, starts
no service and does not rewrite the gateway or relay service configuration. The
returned `installation_hash` pins the completed installation. The Go APIs
`VerifyInstalledSignalRuntime` and `OpenInstalledInboxVerification` inspect it;
the verification instance refuses receiving, listing and acknowledging messages.
An ordinary relay start remains blocked at both paths. Managed Signal account
handoff, endpoint cutover and production activation still need implementation.
Do not remove the fences to enable a receiver manually.

Inspect the original Telegram handoff after gateway publication with:

```bash
deploy notification-worker recovery-telegram-check --local \
  --config /absolute/runtime.json --state-dir /absolute/gateway-state \
  --commit-hash EXACT_GATEWAY_PUBLICATION_COMMIT
```

This offline check compares the published cursor and request seal with live
state under the exclusive worker lease, then checks the observation's ten-minute
lifetime, boot identity, wall/monotonic clocks and five-second cutoff. A missing
operational cursor is an error even when the expected offset is zero. It never
polls Telegram, reads bot credentials, changes offsets or renews old evidence.
Successful JSON contains `intake_enabled: false` and
`requires_activation_recheck: true`: this is an inspection at `checked_at`, not
permission to start a receiver later.

An installed gateway that has completed `adopt-recovery` can renew its Telegram
observation through the managed lifecycle:

```bash
deploy notification-worker renew-telegram --local --root /absolute/installation
# Resume an interrupted operation using the same frozen request:
deploy notification-worker resume --local --root /absolute/installation
```

The operation requires an interactive Telegram account and its private bot token.
It stops the installation's exact managed service, holds the installation and
worker leases, and checks service, configuration and lease identity around each
provider preflight and poll. Stop any other receivers using that bot account
beforehand; this local coordinator cannot establish exclusive ownership against
external consumers. Webhook/account conflicts fail closed.

A request is saved in `operations/ID/telegram-request.json` before polling. Its
stable ID pins the exact predecessor: the original cursor hash for the first
renewal, then the previous `renewal_hash`. Independent completion evidence is
saved in `operations/ID/telegram-receipt.json`; successful `renew-telegram` JSON
includes both the manifest and receipt. Completed retries return the same dated
evidence without polling or reading credentials. A new invocation after completion
creates a new observation. Missing request evidence after polling has started or
missing receipt evidence after publication is an error, not permission to rebuild
history.

The runtime and retained recovery helper must both support renewal. Ordinary
`update` keeps the existing helper. Upgrade older installations explicitly using
an installation spec with the current binary/digest and unchanged runtime paths,
configuration and schema; omit `recovery_commit_hash` from this spec:

```bash
deploy notification-worker upgrade-helper --local --spec /absolute/upgrade-spec.json
```

This retains the old executable/helper, stages and probes both replacements, and
uses the resumable verification lifecycle. A binary rollback cannot cross the
helper upgrade boundary. Use the newly staged helper or current external binary
to resume an interrupted helper upgrade; the old helper does not know the new
operation format. `upgrade-helper` does not migrate state or enable production.

After renewal, only the verification service is restarted. It independently
audits the exact completion while holding its lifetime worker lease. Health
includes `telegram_renewal_id`; updates, compatible binary rollbacks and uninstall
preserve that pin. Verification restart remains possible after observation expiry,
but cannot renew freshness or admit commands. Later production activation still
needs a fresh check and cutoff installation.

Renewal keeps the original publication, canonical inbox, request mappings and
operational offset unchanged. It stores sanitized provider pages in a separate
archive and synchronizes each page plus an independent root checkpoint before
requesting an offset that acknowledges that page. Retries start at zero, including
after an acknowledged request whose response was lost. New inputs remain recovery
evidence and cannot create or confirm commands. The archive retains previous
generations and rejects missing pages, changed identities and conflicting native
message IDs. Per account, bounds are 16 renewals, 4,096 total pages and page bytes limited to
the smaller of `max_database_bytes` and 64 MiB; exhaustion leaves intake fenced.

With the verification service stopped, inspect a completed renewal without polling
or reading bot credentials (restart the verification service afterward):

```bash
deploy notification-worker recovery-telegram-check --local \
  --config /absolute/runtime.json --state-dir /absolute/gateway-state \
  --commit-hash EXACT_GATEWAY_PUBLICATION_COMMIT \
  --renewal-hash EXACT_LATEST_RENEWAL_HASH
```

The check verifies all retained predecessors and page evidence, then applies the
same boot, clock, expiry and cutoff checks to the latest observation. Starting a
successor immediately invalidates inspection of the original or an older renewal,
even if the successor is interrupted. Retain the root
`telegram-renewal-ACCOUNT_HASH.json` and the full
`recovery/OPERATION/telegram-renewals/ACCOUNT_HASH/` archive together with the managed
request/receipt. The managed local Telegram shutdown and verification restart are
implemented. Signal service handoff, installing the renewed Telegram cutoff into
production admission and production activation remain unfinished; renewal itself
grants no intake authority.

The producer reconciliation stage now preserves retained acceptance,
reconstructs missing payloads as ambiguous and records unknown compacted history.
Backup now retains full receipt provenance and can capture immutable
recovery pages while atomically fencing delivery. The target recovery identity is
staged separately; the active binding, old receipt cursor and delivery obligations
remain intact. Legacy acceptance records without complete receipts remain unproven.
Its completion does not enable delivery, publish healthy status or clear LED faults.
A local `backup-worker recovery-notifications` coordinator transfers these pages
to the quarantined worker, evaluates fresh status and durably records the matching completion. Lost responses
and process crashes resume without renewing completed health claims.
Specialist command recovery now also retains original request mappings, results
and receipt provenance under a durable execution fence. The quarantined worker
records these as audit evidence; uncertain jobs and unconfirmed commands remain
blocked. Completion never authorizes execution.
Signal recovery can now export retained relay history past the normal ACK boundary.
The quarantined worker preserves proven request mappings and keeps unknown replays
quarantined; completion does not move operational cursors or enable intake.
Telegram recovery can also import a verified retained gateway snapshot, preserving
command identities, confirmation aliases and original deadlines. Pending inputs
stay quarantined, and recovered commands receive no execution authority. This
offline import neither polls Telegram nor claims complete provider history.
An internal Telegram recovery polling stage can then quarantine returned updates,
persisting pages and a separate progress journal before acknowledging them.
Restart begins at offset zero; damaged or rolled-back progress blocks further
polling. An empty response records a dated observation and keeps intake disabled.
This stage is tested with fake providers and is not wired to a production CLI.
Signal relay snapshots and restore quarantine are also available as internal APIs.
Restore preserves live evidence, stages a separate candidate under a new epoch,
and fences normal relay operations. Interrupted restores resume with the same epoch;
the candidate is not installed or activated.
The quarantined relay can now exchange its old history with the gateway, binding
the snapshot to the staged new epoch. An older relay snapshot produces an explicit
missing-history range while preserving the gateway's retained mappings. The
exchange survives interruption and keeps history incomplete and intake disabled.
Ambiguous outbound deliveries can also receive a journaled local retry/cancel
instruction through the local staging commands `recovery-review` and
`recovery-decide`. These require an already quarantined, reconciled worker.
Review prints the exact payload and evidence bindings, plus any pending or recorded
choice. Save that JSON to a new private file (0600), inspect it, then submit a choice:

```sh
umask 077
deploy notification-worker recovery-review --local \
  --config /path/to/runtime.json --state-dir /path/to/worker \
  --producer producer-id --message event-id > /tmp/recovery-review.json

deploy notification-worker recovery-decide --local \
  --config /path/to/runtime.json --state-dir /path/to/worker \
  --review /tmp/recovery-review.json --action retry \
  --reason "Reviewed the uncertain delivery" --accept-duplicate-risk
```

For a cancellation instruction use `--action cancel` without the duplicate-risk
flag. The actor is the current OS user ID. Reusing the same review and choice
replays the original record after a lost response; a different choice cannot
replace it. A new review exposes an interrupted intent and its original request ID.
Review may initialize local reconciliation metadata, but never records or applies
a choice. Dry-run validates arguments and review-file contents without opening the
worker; it explicitly does not validate current recovery state.
Both commands preserve quarantine; neither applies the choice.
An internal offline ownership transition now binds each completed producer summary
to the new recovery epoch, retains the ownership history and revokes all old
generations. It resumes after interrupted publication without enabling delivery or
changing messages, receipts or recorded choices. It has no CLI entry point yet.
An absent producer can first be fenced independently, without inventing a
reconciliation summary:

```sh
deploy notification-worker recovery-fence-producer --local \
  --config /path/to/runtime.json --state-dir /path/to/worker \
  --producer producer-id
```

This requires an existing quarantine. It moves that producer's ownership to the
new epoch, revokes every old generation and persists `ReconciliationPending`.
Staging and activation remain blocked until the normal challenged producer
exchange completes and its exact summary is bound to ownership. Repeating the
command resumes the same fence; after reconciliation advances, it refuses to
refence the producer. Other producers retain their independent recovery paths.
The command neither publishes recovered state nor enables production service.
Backup now journals this handoff locally, checks matching reconciliation summaries
and resumes lost responses without changing its active identity, receipt cursor or
outstanding delivery obligations.
An internal offline application step can now cancel a reviewed message or mark
an approved retry as fenced, with one durable outcome receipt. An internal
generation step can then stage a fresh generation and bind approved retries to it,
preserving their original envelopes and receipt history. Runtime checks the binding
and ordinary validity, status, route and revision guards before any future grant.
Reviews show pending/applied application and pending/bound retry evidence. These
steps have no CLI command; the generation remains staged and delivery stays fenced.
Backup can now internally commit its delivery identity, staged generation and a
zero cursor for the new receipt stream in one transaction with the recovery mapping.
Old receipts and outstanding obligations survive; recovered submissions are not
enqueued again, and cancellation intents remain pending. The commit requires
matching command-recovery evidence where commands are installed, preserves the
independent command identity, and leaves both delivery and execution fenced.
Command recovery can now internally commit the specialist's new identity together
with a checkpoint of every retained command's result revisions. Original command
and result bodies, receipts and acknowledgement flags remain unchanged. Both
completed and uncertain commands stay fenced; the result outbox also refuses to
expose historical results during recovery. Delivery and command commits can replay
in either order. An internal offline step now installs the reconciled command
outcomes after the source checkpoint commits. Verified terminal outcomes become
visible; uncertain commands keep their admission fence and surviving revision
floors. Original receipts remain historical, and installation creates no new
receipts, replies, confirmations or grants. Its journal supports crash and
lost-response replay. Ordinary command execution/result APIs reject historical IDs.
A separate internal step can seal the recovered request inventory after available
provider and command mappings have been imported. Known requests keep their original
identities, targets and deadlines. Unmapped inputs are durably blocked from becoming
commands, queries or confirmation replies; fresh authorization requires a new request
identity. Signal inventory requires retained relay identities, including for redacted
inbox rows. Sealing does not prove complete provider history or enable intake.
Telegram recovery can now internally commit a cursor handoff bound to the completed
drain and sealed request inventory. Polling restarts at zero; unmapped input at or
before the drain observation plus five seconds is rejected without creating commands
or replies. Commit and activation validation require the same boot, consistent clocks
and an observation less than ten minutes old; replay never refreshes that evidence.
Signal recovery now internally prepares a matching relay/gateway handoff checkpoint
after history reconciliation and request sealing. It binds the original gateway
cursor, exact evidence and intended relay identity. The reserved position is the
larger of the retained relay and gateway watermarks, preventing a restored relay
from reusing positions already known to the gateway. Preparation changes no
operational cursor, sequence or ACK; it is not activation authority. Both retained
and restored relay paths support checkpoint replay after interruption.
Signal cursor installation can now stage a separate relay database and atomically
install the matching relay pin and baseline into the gateway recovery candidate.
The relay candidate retains historical events and the original ACK evidence, with
new positions reserved above both surviving watermarks. Its private database digest
is bound to the gateway's completion record; interrupted installation resumes the
same candidate. The candidate schema deliberately cannot start as a normal relay.
The relay now understands a separately published runtime baseline: older cursors
are rejected, historical events do not occupy pending-queue capacity, and an empty
baseline ACK does not claim historical delivery. Runtime pins require matching file
and SQL evidence. The gateway requires its installed cursor proof and persisted input
beyond the baseline before admitting fresh Signal requests. Staging APIs still do
not publish runtime authority. Version-2 relay snapshots now preserve the baseline
proof, reserved gap, retained event count, actual positions and ACK evidence.
Restoring one stages a separate format-6 quarantine and preserves the live inbox;
interrupted restores resume from a retained immutable source copy. Recovery exchange
and cursor staging now preserve multiple missing ranges across subsequent relay
recoveries. Version-3 snapshots record their separate historical event counts.
Gateway request sealing verifies surviving input against the immutable snapshot
when the relay has lost it; opaque rows remain explicitly missing evidence.
The worker remains quarantined. Managed receiver shutdown and fresh activation checks,
LED ownership reconciliation and managed installation/activation
remain pending; no restore command is enabled.

A synthetic vulnerability specialist tests risk decisions,
rescan jobs and health publication through the same interfaces. Shared backup
notification migration and report-service installation, a real vulnerability scanner,
recovery tooling and production activation remain
unfinished. See the interactive plan for staging commands and the
[backup command configuration example](examples/notification-worker/backup-commands.json).
Local tests do not establish live provider or ARM64 qualification.

In the staged runtime, Telegram/Signal intake, command conversion and command
replies retry independently with bounded backoff. Expiring loop observations are
included in status; an intake outage does not stop unrelated notification delivery.
The LED reconciler verifies registry state and daemon arbitration after updates.
Status includes optional `led` diagnostics: `registry_accepted` confirms observed
registry presence, `suppressed` means another state wins, and `visual_verified`
remains false. These observations expire after at most 30 seconds. Registry errors
stop healthy renewal and attempt to clear the lease; if the clear cannot be
confirmed, the bounded lease expires. Another application's states are never evicted.

Before clearing an obsolete fault, the runtime checks the caller-owned daemon
entry against its recorded LED, style and persistence, then rechecks current
source evidence. A conflicting registry entry or failed lookup preserves the
cleanup obligation and blocks healthy renewal. An already absent owned fault can
be cleared idempotently; revoking a healthy lease does not depend on registry lookup.

Staged LED bindings are available in the
[LED runtime example](examples/notification-worker/led-runtime.json). Configure
`worker.led_bindings` before initializing a disposable worker: each entry selects
an enrolled producer, exact subjects or a sole `"*"`, and `green` or explicitly
opted-in `red` (`allow_power: true`). The daemon must already enable the target;
deploy does not change its configuration. Optional `styles` choose named patterns
and priorities within severity bands: baseline 0–9, healthy 10–39, pause 40–59,
warning 60–79, unknown 80–89 and critical 90–100. There are at most 32 bindings and
512 selector entries in total. `led_bindings` in status shows source conditions
and contributing subject keys; `omitted_subjects` counts labels omitted from the
bounded summary. `led_outputs` shows each binding's observed registry state.
Wildcards with more than 128 active subjects report `overflow` and cannot establish
healthy coverage; known critical faults remain visible. Missing selected subjects
remain unknown. Pauses expire with their evidence;
fresh retirement clears faults without asserting health.

Schema 7 pins the mapping and runtime UID. Changing or removing an initialized
binding requires an explicit migration; do not delete the state directory to
bypass this check. The implicit worker aggregate remains enabled alongside
configured bindings.

Schema 8 records each LED mutation intent before its RPC and its acknowledgement
or uncertain outcome afterward. It keeps 64 recent attempts per binding and the
latest acknowledged operation; `led_operations` in status includes pending and
uncertain counts, the pruned history boundary, and how many unconfirmed attempts
were pruned. This is historical evidence, not proof of current registry presence
or visible output. Restart derives new work from current source evidence and never
replays the history. Healthy/pause TTLs are recalculated after the intent commits.

Schema 9 adds a local migration command. Keep the original runtime JSON. Resolve
or retire all fault/cleanup records for each binding you will change or remove,
using the original runtime. Then stop the staged worker and prepare a second
runtime JSON changing only `worker.led_bindings`. Run:

```sh
deploy notification-worker led-migrate --local \
  --config /private/worker/runtime.json \
  --next-config /private/worker/runtime-next.json \
  --state-dir /private/worker/state
```

The UID, LED socket and other runtime settings must remain unchanged. The command
checks daemon targets, patterns, namespace collisions and capacity, revokes old
healthy leases, and establishes the new unknown baselines. It preserves unrelated
states and cannot clear fault incarnations. It does not stop or start services.
After success, restart the staged worker using the new config; fresh producer
reports are required before healthy indications can resume.

If interrupted or a daemon acknowledgement is lost, rerun the exact command with
the original two config files. Startup is blocked until the journal completes;
deleting its file does not bypass the database fence. If the daemon loses its
baselines, resume recreates the baseline sets without replaying old baseline clears.
Dry-run validates the configs only and explicitly reports that it has not inspected
worker state or the daemon.

Previous mappings and their retained operation histories are archived transactionally.
Status includes a `layout_hash` alongside operation counters so a new mapping's
counters cannot be confused with the previous mapping. Returning to an earlier
config creates a new mapping revision. History currently supports 16 migrations;
archive compaction is not yet provided, and further migrations fail before LED I/O
when the limit is reached. Older binaries refuse schema 9. Schema-6/7/8 snapshots
remain readable; restored history retains its original epoch in quarantine.

An already quarantined local recovery can now collect a durable LED ownership review:

```sh
deploy notification-worker recovery-led-review --local \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state
```

This requires a configured absolute `led_socket`, the existing recovery quarantine,
and an available worker lease. It does not create a quarantine or stop services;
there is still no operator restore/activation command. It compares the restored
reconciliation database, the preserved original database (including newer faults),
and read-only daemon observations. JSON output distinguishes snapshot-only and
post-snapshot faults, cleanup intents, mapping revisions, unrecognized states and
foreign UIDs. Schema-6/legacy faults cannot establish exact epoch ownership.
Incompatible configurations, damaged ownership history and missing source databases
fail closed. Free-form daemon messages are hashed, not copied into the review.

Repeated calls reuse the sealed observation. `--refresh` explicitly captures another,
up to eight inventories per recovery within storage budgets. An interrupted capture
is sealed first, even with `--refresh`; rerun afterward to request a new capture.
Restart never renews an observation's timestamp. `observation_current` requires
unchanged source evidence, the same boot and less than 30 seconds of elapsed time;
it is not a claim that the registry stayed unchanged. The review retains exact old
fault identities and cannot adopt ownership, clear faults, publish health or activate
the worker. `clear_allowed`, `activation_allowed` and `visual_verified` remain false.
Dry-run validates configuration only, without inspecting state or contacting the daemon.

The reviewed fault union can now be frozen for later recovery publication:

```sh
deploy notification-worker recovery-led-preserve --local \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state \
  --inventory n-REPLACE_WITH_REVIEW_ID \
  --inventory-hash REPLACE_WITH_REVIEW_HASH
```

The first call requires the current, sealed 30-second inventory and rechecks known
fault instances against the registry. It preserves the complete union from both
ledgers, including post-snapshot and snapshot-only faults, with their original
IDs, epochs and cleanup history. Unrecognized and foreign states remain unowned;
free-form daemon messages are never copied. Mismatched mappings, UIDs, legacy
ownership, changed sources or fault instances prevent preparation.

An independent file intent and SQLite acknowledgement make retries crash-safe.
Repeat the exact ID/hash after interruption; a retained intent can finish after
expiry or reboot without contacting the daemon or refreshing its evidence. Once
prepared, a different inventory cannot replace it. `recovery-led-review` exposes
its `preservation` summary and source-drift status. The record is bounded to one
per recovery and at most 2,048 proven fault identities, subject to storage limits.

This prepares ownership provenance for the publication coordinator. It leaves
both source fault ledgers and daemon states untouched, grants no clear/healthy
permission, and cannot discharge domain faults or remove quarantine. Normal-runtime
acceptance, runtime-wide reconciliation and final publication remain
unfinished; a committed preservation record is not production activation.

The preserved union can be applied to the quarantined reconciliation database:

```sh
deploy notification-worker recovery-led-adopt --local \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state \
  --inventory n-REPLACE_WITH_CURRENT_REVIEW_ID \
  --inventory-hash REPLACE_WITH_CURRENT_REVIEW_HASH \
  --preservation-hash REPLACE_WITH_COMMITTED_PRESERVATION_HASH
```

This requires the exact committed preservation and a current sealed inventory,
with a fresh registry check. If the original observation expired or separately
authorized cleanup changed the registry, refresh the review first. Adoption keeps
every original fault ID and epoch and sets all executable cleanup flags to false;
their original values remain in the preservation evidence. Faults cleared by a
separate domain-authorized operation remain in the ownership history and are not
recreated. Foreign registry entries are never adopted.

Ledger replacement and its acknowledgement commit in one SQLite transaction. An
independent intent makes interrupted adoption resumable using the exact same three
arguments, even after expiry/reboot, without repeating registry requests. A retry
cannot select another inventory or change the preserved union. Review includes an
`adoption` summary. Subsequent reviews retain the original snapshot/source evidence
and verify the merged ledger against the adoption checkpoint; missing or changed
rows block reconciliation. Adoption grants no cleanup or activation authority and
leaves the daemon and original live database untouched. The worker stays quarantined
until the remaining reconciliation and publication work is implemented.

After adoption, archive the old operation and mapping journals before starting
history for the new recovery epoch:

```sh
deploy notification-worker recovery-led-handoff --local \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state \
  --adoption-hash REPLACE_WITH_COMMITTED_ADOPTION_RECORD_HASH
```

This requires the current database schema, unchanged configuration, UID and LED
socket, and the exact adoption record. It archives the original journals, layout
and adopted fault union, then resets only the active operation/mapping histories
in one transaction. Original fault IDs remain retained and protected from ordinary
cleanup. No daemon request is sent. The independent intent supports exact retries
after a crash; retrying cannot erase operations already recorded in the new epoch.
Review exposes `epoch_handoff`; its fault count records the union retained at
handoff, before any later cleanup imports. Missing or changed archive/provenance records and
preserved-source drift block the handoff. The candidate remains quarantined;
worker-wide fault reconciliation and final publication are still unfinished.

Recovery now also has an internal producer evidence exchange
(`OpenRecoveryLEDEvidenceSession`). A fresh challenge pins the inventory, both
source ledgers, producer domain history and earlier recovery reports. The producer
must return a newly evaluated full snapshot and, for policy faults, transitions
for the exact retained fault revisions. A newer `ok` status alone cannot discharge
a policy fault. Missing subjects, ongoing faults, expired support, changed sources
or regressed revisions block the proposal. Explicit retirement can resolve a
binding without asserting health.

`recovery-led-review` includes a `resolutions` section when this exchange has stored
evidence. A resolved binding lists exact `proposed_fault_ids`, an evidence hash and
an expiry capped by its shortest lease and the inventory window. These are
proposals only: no LED or ownership ledger is changed, and `clear_allowed` remains
false. The worker-wide aggregate requires separate runtime reconciliation.
Challenges and reports survive interruption without new timestamps; refresh cannot
discard earlier producer evidence or acceptance dependencies. The exchange is an
internal building block. Backup now has a coordinator that evaluates its actual
archive, incidents, host probe, qualification and pause/run state while delivery
remains fenced. It requires completed producer reconciliation, stores the new
status sequence and frozen report atomically, and records the worker acknowledgement
separately. Lost replies reuse that report; a new inventory requests a new evaluation.
Expired probes, unresolved incidents or missing qualification cannot become healthy
evidence. Backup produces ordinary resolution/retirement evidence, not risk-acceptance
transitions. Its bounded journal retains up to eight reports for the recovery.

To reconcile a backup producer with an already quarantined notification worker,
use its exact recovery operation ID (`operation` in the preparation output or LED recovery review):

```sh
deploy backup-worker recovery-notifications --local \
  --config /private/backup/worker.json --state-dir /private/backup/state \
  --notification-config /private/worker/runtime.json \
  --notification-state /private/worker/state \
  --operation n-REPLACE_WITH_RECOVERY_OPERATION
```

This command drains the local report pump through its lease, fences backup
notification ownership, freezes the producer export, and exchanges retained
messages/receipts and an evaluated snapshot with the offline worker. It prints
the durable reconciliation summary and its `summary_hash`. Interrupted exchanges resume; a completed
exchange returns the same historical summary without refreshing its evidence.
A different recovery operation is rejected before the producer is fenced.
Neither provider delivery nor command recovery, ownership handoff, active cursor
publication or service startup is performed. Prepare quarantine first with
`notification-worker recovery-prepare`. Final recovered-state publication and
managed service orchestration remain unfinished.

After reviewing that completion, perform the local producer handoff:

```sh
deploy backup-worker recovery-handoff --local \
  --config /private/backup/worker.json --state-dir /private/backup/state \
  --notification-config /private/worker/runtime.json \
  --notification-state /private/worker/state \
  --operation n-REPLACE_WITH_RECOVERY_OPERATION \
  --summary-hash REPLACE_WITH_RECONCILIATION_SUMMARY_HASH
```

The coordinator keeps one offline gateway lease throughout the handoff. For an
interactive producer it also holds the existing backup command-runner lease and
requires the command ledger's original archive binding. It reconciles retained
commands, revokes old delivery generations, stages one new generation, commits
the new delivery identity with a zero unread-receipt cursor, and installs proved
command outcomes under the new identity. Old payloads, result receipts and
request mappings remain history. Previously successful commands stay successful;
unfinished commands stay uncertain, with their execution and confirmation fences
intact. No domain command runs and no receipt is acknowledged by this operation.

Missing command state or a missing command lock is refused for an interactive
producer. Removing command enrollment cannot hide a surviving command ledger.
Notification-only producers can hand off when both configurations and retained
state prove that no command ledger is involved. Neither path initializes state,
starts services, sends messages or releases quarantine. The JSON result includes
delivery and command checkpoints with both activation flags false. Repeating the
exact command resumes partial completion or returns the same historical result;
it cannot renew evidence or create another generation. Managed service handoff,
runtime configuration installation and final recovered publication remain pending.

The local backup LED coordinator is available after this producer reconciliation.
These local recovery commands use existing state and the installed backup configuration
(an owned regular file with mode `0600`). They never initialize missing state,
prepare quarantine or start services:

```sh
deploy backup-worker recovery-led-evidence --local \
  --config /private/backup/worker.json --state-dir /private/backup/state \
  --notification-config /private/worker/runtime.json \
  --notification-state /private/worker/state \
  --inventory n-REPLACE_WITH_CURRENT_REVIEW_ID \
  --inventory-hash REPLACE_WITH_CURRENT_REVIEW_HASH
```

Run `notification-worker recovery-led-review` again to inspect the resulting
binding proposal. After preservation, adoption and LED epoch handoff are committed,
clear and import one exact reviewed backup fault:

```sh
deploy backup-worker recovery-led-clear --local \
  --config /private/backup/worker.json --state-dir /private/backup/state \
  --notification-config /private/worker/runtime.json \
  --notification-state /private/worker/state \
  --inventory n-REPLACE_WITH_CURRENT_REVIEW_ID \
  --inventory-hash REPLACE_WITH_CURRENT_REVIEW_HASH \
  --binding REPLACE_WITH_BACKUP_BINDING \
  --fault deploy.f.REPLACE_WITH_EXACT_48_HEX_FAULT_DIGEST \
  --evidence-hash REPLACE_WITH_REVIEWED_EVIDENCE_HASH \
  --epoch-hash REPLACE_WITH_COMMITTED_EPOCH_HANDOFF_HASH
```

The clear command pins the backup producer and its configured binding, retains
the backup source leases across the guarded clear, and imports only a committed
`acknowledged` or domain-authorized `observed_absent` result. Output includes the
cleanup result/hash and, if completed, the import decision. While the reviewed
inventory remains current, a lost response can be retried with the same arguments
without sending another clear, even after the original evidence expires. Uncertain or
not-sent outcomes remain unimported; inspect their outcome before deciding on a
new attempt. If clear succeeded but import failed, the reported result hash can
also be passed to `notification-worker recovery-led-import-cleanup`.

Current evidence has a short lease. If the inventory expires, refresh the LED
review and obtain a new backup evidence report before a new clear attempt. A
refresh does not replay an earlier attempt. Exact historical completion does not
renew its proof or assert current backup health. Both commands support global
`--dry-run`; it validates arguments/configuration without opening state or making
registry requests. Other producer adapters, remote service orchestration and final
recovery publication remain pending. Running the review command alone does not
obtain producer proof.

Exact named-binding cleanup is now available to internal coordinators through
`OpenRecoveryLEDCleanupSession`. It requires the reviewed inventory/evidence hash,
fresh exact-UID registry checks and the producer's current domain guard. Backup
holds both source leases and uses a speculative evaluation that always rolls back.
The clear intent and outcome are journaled independently; restart never replays an
uncertain attempt. Another attempt requires a fresh inventory and fresh producer
evidence. No source ownership ledger, delivery cursor or activation fence changes.
Review reports `cleanup_attempts`, `cleanup_unconfirmed` and up to 64
`recent_cleanup` outcomes. Acknowledgement means registry acceptance, not physical
rendering. There is deliberately no chat command that bypasses these checks.

Once a named-binding clear has a committed `acknowledged` or domain-authorized
`observed_absent` result and the epoch handoff is committed, retire its exact
inherited ledger row:

```sh
deploy notification-worker recovery-led-import-cleanup --local \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state \
  --epoch-hash REPLACE_WITH_COMMITTED_EPOCH_HANDOFF_HASH \
  --result-hash REPLACE_WITH_EXACT_CLEANUP_RESULT_HASH
```

Review lists up to 32 distinct eligible receipts in `importable_cleanup`, with
the full count in `importable_cleanup_count`. After importing, review again for
the next candidates. `cleanup_imports` retains the exact import decisions. Each
decision pins the original fault, cleanup receipt, domain proof and epoch archive.
Independent intent/history and atomic row removal support exact crash resume.
Retrying imports the same historical result without renewing proof or contacting
the daemon. Pending imports must finish before runtime ownership is accepted.
Uncertain/not-sent outcomes cannot be imported; an ordinary registry absence is
not a cleanup receipt. Missing or changed history fails closed. Newer fault IDs
remain fenced, and the full original union stays archived. Imports make no domain
or healthy-status claim and do not release quarantine. Worker-wide aggregate
cleanup and managed producer cleanup orchestration remain unfinished.

The mutation path passed seven SIGKILL cases and integration against the pinned
pi-status-led daemon's mock hardware driver. Physical LED testing remains deferred.
Managed LED-binding migration in verification mode is described below. Production
activation, UID/socket changes and runtime-wide fault reconciliation remain
unfinished. Runtime ownership primitives now verify inherited faults against the
committed adoption/preservation/inventory chain and report `inherited_led_faults`
per binding. Such faults keep output critical or unknown, cannot be reused as new
incarnations, and are excluded from normal Set/Clear and cleanup selection.
Missing or changed proof fails closed. These primitives are tested using a fenced
candidate; worker-wide reconciliation and final recovered-state publication are
still required before a recovered service can run.

A staged worker can also run in explicit verification mode:

```sh
deploy notification-worker serve --local --verification \
  --config /private/worker/runtime.json \
  --state-dir /private/worker/state \
  --socket /private/worker/state/api.sock
```

It accepts durable producer submissions and exposes status while withholding
message delivery, chat intake, command execution and reply grants. LED reconciliation
revokes healthy leases and maintains unknown baselines; existing fault IDs remain
untouched even after healthy producer submissions. Status includes
`verification_only: true`. This mode applies to that process and leaves canonical
producer ownership unchanged. It is not a durable installation/activation fence.

`install.RenderVerificationService` renders a native user-service template with
that mode, resource limits, private file permissions, Unix-only networking,
private devices, source-path exclusions and writable access restricted to worker
state. The local managed lifecycle now installs this verification mode through
`setup`/`update --local --spec …`, and supports `resume`, `uninstall` and
`installation-status --local --root …`. It pins releases, runtime, identity and an
independent recovery helper, journals service/file transitions and checks the exact
running installation revision. It adopts explicitly initialized inactive state;
it never recreates missing state or implicitly upgrades schemas. Uninstall retains
state, pending obligations, credentials and LED fault history. See the
[native verification installation guide](examples/notification-worker/INSTALLATION.md)
and [installation spec](examples/notification-worker/installation.json).

`queue-inspect` and `receipts-inspect` provide bounded, producer-scoped inspection
through the Unix API. Their pagination positions cannot advance a producer's
listed/acknowledged receipt floors; inspecting history does not authorize an ack.
Queue inspection omits message text. These are live views, not recovery exports.

`producer-retire --local --config … --state-dir … --producer … --reason …`
previews permanent retirement of an enrolled producer epoch; `--apply` publishes
the decision under the stopped worker's exclusive lease. All generations must be
revoked, subjects explicitly retired, and delivery/command/receipt/policy/LED
obligations settled first. It preserves history, blocks new work and activation,
and removes the retired producer's heartbeat requirement without asserting health.
Configuration enrollment remains retained. See the
[producer retirement guide](examples/notification-worker/ADMINISTRATION.md#producer-retirement).

`receipts-compact --local --config … --state-dir … --producer …` previews bounded
receipt cleanup; `--apply` deletes acknowledged intermediate history older than
the configured audit window (90 days by default). Latest outcomes, administrative
audit receipts, message replay fences, cursor values and chat contexts survive.
Inspection reports discarded-history counts without granting acknowledgement
authority. See the [receipt compaction guide](examples/notification-worker/ADMINISTRATION.md#receipt-compaction).

`rollback --local --root … --from-revision …` re-releases a retained executable as
a new managed verification revision, preserving current configuration, schema and
state. It uses the update journal and refuses changed/incompatible artifacts.
Interrupted rollback resumes through the retained helper; an unfinished update
can first be cancelled with uninstall. See the
[installation guide](examples/notification-worker/INSTALLATION.md).

`upgrade-helper --local --spec …` explicitly stages a new runtime and recovery
helper at the current schema, retaining configuration, identity and completed
recovery/Telegram evidence. Ordinary updates keep their pinned helper. This gives
older installations a path to new lifecycle capabilities without a data migration.
See the [helper upgrade instructions](examples/notification-worker/INSTALLATION.md#explicit-helper-upgrade).

`migrate-schema --local --spec …` explicitly upgrades an installed verification
worker from schema 9 to 10. It pins the same identity/configuration/exclusions,
checks migration-capable executables before stopping the service, and retains the
original database plus both recovery helpers. SQL migration and its checkpoint
commit atomically; interrupted work uses `resume` with the staged new helper.
Ordinary updates still cannot change schemas, and rollback cannot cross this
migration. See the [schema migration instructions](examples/notification-worker/INSTALLATION.md#explicit-schema-migration).

`migrate-led --local --spec …` changes only LED bindings on an installed
verification worker. It pins the identity, socket, enrollment, providers and
limits; checks migration-capable executables before service stop; and journals
the mapping under the same installation operation. Changed bindings must have
no remaining fault/cleanup records. The migration can revoke healthy leases and
set unknown baselines; it cannot clear faults or enable healthy output. Exact
resume uses the staged new helper. See the
[LED mapping migration instructions](examples/notification-worker/INSTALLATION.md#managed-led-binding-migration).

`qualification-template`, `qualify --report …` and `qualification-status` now
record bounded, dated operator evidence for the exact managed revision, local
configuration, credential fingerprints, architecture and UID. Partial/failed
reports are retained; changed bindings and expired reports cannot count as current.
These commands perform no live checks and grant no activation authority. See the
[qualification evidence guide](examples/notification-worker/QUALIFICATION.md).

`activation-review --local --root …` composes the current revision, qualification,
provider bindings, retained recovery/cursor evidence and command prerequisites
into a read-only JSON review. Worker-state checks require an inactive gateway and
exclusive leases; the command never stops it. Managed Signal verification health
and current Telegram handoffs are checked without provider polling. Stale outbound
Signal pins and unfinished production transitions remain explicit blockers. All
activation/delivery/intake/execution flags remain false. See the
[admission review guide](examples/notification-worker/QUALIFICATION.md#activation-admission-review).

`activate-delivery --local --root … --manifest-hash … --qualification-hash …`
now journals dispatcher activation for initialized outbound-only Telegram workers.
Exact current qualification, permanent file/SQL ownership, executable capabilities,
loaded service/process pins and activation health guard the transition. Runtime
rechecks admission periodically and before sending. `deactivate-delivery` returns
to verification even after credential loss or qualification expiry; ownership
survives update, compatible rollback and uninstall. Producer generations remain
inactive and need separate explicit admission. Recovered/Signal, command/LED and
legacy backup cutover remain unfinished. See the
[delivery activation guide](examples/notification-worker/DELIVERY_ACTIVATION.md).

`migrate-signal-profiles --local --spec …` derives outgoing Signal conversation
pins from an adopted supervised endpoint and freezes a separate effective runtime.
It retains canonical recovery bytes, queued route snapshots and Telegram evidence;
affected routes get a new revision. Capability checks, lifecycle resume and runtime
health bind the transition. Production activation and the Signal runtime handoff
remain open. See the
[profile migration guide](examples/notification-worker/SIGNAL_VERIFICATION.md#outgoing-profile-migration).

Signal linking now enters the same account lease as both relays through the
pinned container's deploy entry point. Interrupted or failed attempts block
relinking and relay startup until explicit account-state review. Local
`signal-link-status`, `signal-service-status` and `signal-link-resolve` expose the
link and service journals. Backup linking now stops its enrolled legacy relay and
restores its recorded prior activity, with crash-resumable maintenance fencing.
The internal recovery coordinator now durably fences the account before stopping
its pinned service, checks surviving CLI children and installs/verifies the
recovered inbox under the gateway lease. `signal-recovery-status --local
--state-dir …` inspects the fence. Completion keeps ordinary relay startup and
linking disabled. `recovery-signal-handoff --local --spec …` now connects that
coordinator to an existing native user-systemd relay: it pins the loaded unit,
executable, configuration and live account/inbox leases, then stops it and installs the
offline inbox. Exact retry handles interrupted stops without starting a receiver.
`recovery-signal-podman-handoff --local --spec …` now provides the corresponding
rootless backend for the supported interactive Quadlet profile, including exact
container/image IDs, loaded commands, namespace-visible mounts and live flock
proofs. The tested flow preserves the fence after container removal and handles
lost stop responses. `signal-verification-prepare` now enrolls a separate,
health-only endpoint and stages the next verification gateway configuration.
`signal-verification-serve` runs that endpoint in the foreground, holding the
account and both inbox leases and continuously checking retained evidence. The
staged configuration can now be adopted with `adopt-signal --local --spec …` into
an existing recovered gateway verification service. Its journal preserves endpoint
pins across restart/update/rollback, and the gateway stops serving if the separate
verifier becomes unavailable or mismatched. The original account-handoff inputs
remain intact. `signal-service-setup --local --spec …` now installs a separate
native verification service with pinned executable/publication and resumable
setup/removal. `supervise-signal` binds an already adopted foreground endpoint to
that service; new adoptions can include the service pins directly. Gateway units
then depend on the verifier, and readiness includes its exact service hash.
Real user-systemd qualification, legacy backup-relay adoption and production
activation remain unfinished.
See the [Signal verification endpoint guide](examples/notification-worker/SIGNAL_VERIFICATION.md).
Details: [Podman Signal recovery](examples/notification-worker/SIGNAL_PODMAN_RECOVERY.md)
and the [Signal maintenance guide](examples/notification-worker/SIGNAL_MAINTENANCE.md).

For a stopped worker, `message-review`, `message-decide` and
`message-decision-status` provide audited retry/cancel administration. A private,
five-minute review pins the exact event, route, generation, configuration, epoch
and receipt history. Retrying an ambiguous event requires explicit duplicate-risk
acknowledgement. Retries retain attempt counts and normal validity/ownership guards;
cancellation never means acceptance. Repeating a committed decision returns its
original result without reopening the event. The commands neither stop services
nor call providers. See the [administration guide](examples/notification-worker/ADMINISTRATION.md).
Administrative audit requires storage schema 10; older binaries refuse it.
Managed administration will not implicitly upgrade older state. Compatible
unmanaged staging upgrades are transactional, and older snapshots remain readable
in quarantine. The explicit managed schema 9 → 10/helper migration is available;
other schema transitions and configuration migrations remain unfinished.

Recovered/Signal activation, producer cutover, command/LED admission, other
managed schema/configuration migrations, Signal
service/account handoff and later recovery epochs remain unfinished. Offline systemd
validation and temporary executable/socket tests do not establish that all
restrictions work in a real user manager.

### Managed inventory

```sh
deploy ls                          # Current local account only
deploy ls vps-main                  # Named server account
deploy ls --managed vps-main
deploy ls --json vps-main
```

The probe requires Python 3, Podman, and systemd on the inspected host. It is sent
over the existing authenticated SSH connection without installing a helper.
It reads Quadlet search paths, generated service properties, and Podman inspect
data. It does not reload systemd, start services, enroll identities, or adopt files.
Output separates management, ownership evidence, and runtime state. An effective
generated unit's container ID can associate a custom container name; similar
names or copied labels cannot. Unproven associations remain separate rows.

Application, Traefik, and maintenance renderers use one versioned JSON metadata
encoder. Application markers travel with the unit through existing release and
rollback transactions. User labels under `io.deploy.*` are reserved. A marker is
a public management claim, not export authorization. Matching committed service
and target ownership is reported separately. An `enabled` backup column indicates
opt-in only; the exporter revalidates the binding and ownership under
the deployment locks. Drop-ins and unresolved effective configuration are shown
as diagnostics rather than interpreted as safe export policy.

JSON has `schema`, stable entry IDs, `complete`, and diagnostics. Failed probes
produce partial results and a nonzero exit status, never a healthy empty list.
`--managed` retains diagnostics for filtered rows. The bounded probe supports up
to 512 source files/containers/units, 1 MiB per Quadlet, and 16 KiB per marker.
Unknown metadata schema versions, malformed markers, shadowed sources, masks,
and unassociated containers stay visible. Listing is an observation; it does not
hold deployment locks or claim a transactionally consistent source inventory.

Local SQLite snapshot creation now rejects uninitialized sources, normalizes
private WAL-derived snapshots to DELETE mode, and validates them without creating
sidecars. Generic checks disable SQLite startup scripts, use bounded output and
process budgets, and distinguish unsupported validator features from integrity
findings. These checks are not a filesystem/network sandbox or an application
restore test. The dedicated worker validator still needs platform qualification.

Run `make test-backup-worker` for the worker and foundation checks, or `make test` for the
complete unit/process suite, race detector, and vet. In a workspace with a
read-only home cache, set `GOCACHE` and `XDG_CACHE_HOME` to writable temporary
directories. `make test-backup-worker-isolated` checks staged SSH trust isolation,
automatic update recovery (including trust files) after SIGKILL, and the
production SQLite validator, capture-lock inheritance through Podman unshare, and
native SQLite watchdog behavior after supervisor death in a disposable rootless
Podman/systemd environment with its own store. Full backup flows, VM reboot/power
loss, and live ARM/provider acceptance gates remain outstanding.

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
recovery, corrupt inventory, or a checksum mismatch in any matching snapshot
(including retained copies) stops cleanup. Older snapshots without managed metadata
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

### Code layout

The repository uses one Go module. `cmd/deploy` is the canonical executable entry
point; root `main.go` forwards to the same CLI so existing `go build .` and
`go install github.com/dryaf/deploy` commands continue to work.

| Directory | Responsibility |
| --- | --- |
| `internal/cli` | Command parsing and dispatch |
| `internal/config` | Strict configuration, server inventory resolution, backup policy |
| `internal/deployment` | Application/server operations, database operations, retention, embedded transaction scripts |
| `internal/quadlet`, `internal/inventory` | Unit rendering/metadata and discovery |
| `internal/backup` | Archive, worker lifecycle, state transactions, outbox, recovery and restore |
| `internal/backup/source` | Restricted source helper, capture and source enrollment |
| `internal/backup/protocol` | Snapshot framing and wire types |
| `internal/backup/notification` | Notification providers, heartbeat and Signal relay |
| `internal/backup/helper` | Installed helper artifacts |
| `internal/safefs`, `internal/sqlite` | Filesystem/lock ownership and bounded SQLite snapshots |
| `internal/process`, `internal/transport`, `internal/ui` | Process execution, SSH/rsync and CLI output/options |
| `internal/testutil` | Shared test fixtures |

Archive control, worker state and the notification outbox stay in one package
because their transactions and locking rules are coordinated. Tests live beside
the code they exercise and are grouped by feature. Embedded shell/Python assets
live in the package that embeds them.

`make build` builds `./cmd/deploy`. `make test` runs every package's unit tests,
race tests and vet checks. The isolated integration scripts build their tagged
test binaries from `internal/deployment` or `internal/backup`.

### Release verification

`make release-check` requires the normal/race/vet suite, both isolated rootless
Podman suites (image deployment and backup worker recovery), and the VM interruption
matrix. `make test-backup-worker-isolated` runs the backup suite separately.
There is no checked-in CI workflow; CI and branch/release protections must be
configured separately to enforce this gate before publishing.

`make test-deploy-vm-crash` builds a disposable bootable VM from the pinned integration
base, using a temporary disk and generated credentials. It power-cuts and reboots
both deployment modes after pending publication, database snapshot, candidate
activation, database-rewind journaling, commit and pending removal, then verifies
files, database contents, application responses and deployment identity. QEMU uses
KVM when available and otherwise uses software emulation. This test requires the
QEMU tools and enough temporary storage; it never boots or modifies a user's VM or
accesses production configuration.
