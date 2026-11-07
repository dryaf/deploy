# Development and testing

## Documentation checks

Run `make docs-check` after editing or moving documentation. It checks local links
and headings and verifies the generated worker configuration reference. It does
not contact external sites or run race tests. Use `make worker-reference` to
refresh the generated field table after changing public configuration fields.

Keep user guides under `docs/`, configuration assets under `examples/`, and the
root README as a short entry point. Track unfinished work in
[the roadmap](../roadmap.md). Retire completed plans after preserving any lasting
contracts in the design/reference docs; keep active plans only while needed.

## Code layout

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

`make build` builds `./cmd/deploy`. `make test` runs every package's regular tests
and vet checks. `make test-all` runs those checks and then the race suite. Race
checking is deliberately opt-in during development, not an every-iteration gate.
Regular tests have a 35-minute package limit for the retained-history/crash fixtures
and the compiled SSH workflow scenarios;
this is an upper bound, not a forced wait, and normal Go test caching remains
enabled. The isolated integration scripts build their tagged
test binaries from `internal/deployment` or `internal/backup`.

## Release verification

`make release-check` depends on `test-all` and requires the normal/race/vet suite,
both isolated rootless
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
