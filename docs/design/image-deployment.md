# Image deployment design

This is the retained contract for registry-image deployment. The original
implementation plan is retired. For commands and examples, read
[application deployment](../applications.md); for outstanding qualification, see
the [roadmap](../roadmap.md).

## Scope

An image project needs configuration and Git versioning, but no Go source,
`go.mod`, binary, Dockerfile or placeholder artifacts. It uses the existing SSH,
rootless Podman, Quadlet, release, status and log workflows. Binary deployment
remains the default when no mode is specified.

Application integration, PDF layout and application database migrations belong
to the application project. The [Gotenberg example](../examples/gotenberg.md)
illustrates an internal service; it is not a deployed infrastructure project.

## Configuration contract

```yaml
deployment:
  mode: image
```

The mode applies to every environment; each environment selects its own image.

| Setting | Binary mode | Image mode |
| --- | --- | --- |
| `binary_name` | Required | Not required |
| `build` | Go build or configured command | Explicit build settings are rejected |
| `quadlet.image` | Name for the remote build | Fully qualified registry reference with explicit tag or digest |
| `quadlet.dockerfile` | Existing `Dockerfile.vps` default | Explicit Dockerfile is rejected |
| `artifacts.include` | Existing defaults | Empty unless explicitly configured |
| `quadlet.exec` | Existing behavior | Optional; otherwise use image startup defaults |
| `build.arch` | Selects build architecture | Not applicable; Podman selects the target platform |

Reject short image references and embedded registry credentials. Pull the selected
reference for each release, then pin the resulting local image ID. Later changes
to a registry tag cannot change an installed release or its rollback image. Keep
Git release identity, registry digest and local image ID separate.

### Routing, environment files and networks

Omitted `router.enabled` retains inference from `domain`, `host` or `rule`.
Explicit `true` requires a routing target. Explicit `false` emits
`traefik.enable=false`, with no router labels or Traefik service dependency.
Generated labels override conflicting keys while preserving unrelated labels.

| `quadlet.env_file` | Result |
| --- | --- |
| Omitted in binary mode | `<target_dir>/.env` |
| Omitted in image mode, no `sync_env_file` | No environment file |
| Omitted in image mode with `sync_env_file` | `<target_dir>/.env` |
| Explicit empty string | No environment file; reject simultaneous `sync_env_file` |
| Nonempty path | Resolve relative to `target_dir`, or use the absolute remote path |

`sync_env_file` writes `<target_dir>/.env`; reject a conflicting environment-file
path. A separately managed remote file can be referenced without synchronization.
Required files must exist before activation; never create an empty substitute.

Render `network_aliases` as repeated `NetworkAlias=` entries. Keep the container
name `systemd-<service_name>`. Production and test environments sharing a network
need distinct aliases. Existing network Quadlets can be dependencies even when
Traefik routing is disabled. Deployment does not create network infrastructure.

### Health and rendering

`health_cmd` runs inside the container through `podman healthcheck run`.
`health_url` is checked from the VPS host. If both are configured, both must pass.
Periodic Podman health checks remain enabled separately.

`health_timeout` is a positive duration bounding readiness, including individual
checks. The original defaults are approximately 30 seconds for binary releases
and 60 seconds for image releases. Without an application check, report that only
service activity was verified. See the application guide for the later readiness
window and execution-budget settings.

Render the candidate from normalized configuration. Use the resolved local image
ID with `Pull=never`; omit `Exec=` when image defaults apply. Put restart behavior
and CPU limits in their correct systemd sections. Validate with the target's
actual Quadlet generator; online documentation alone is not compatibility proof.

## Validation and execution boundaries

Validate before builds, Git publication, SSH, service changes or file writes.
Reject conflicting options and invalid references, durations, paths, service
names and aliases. Values must not inject shell commands or Quadlet lines. Parse
OCI references without silently qualifying short names.

Separate planning, rendering and execution. Transaction helpers return errors
rather than exiting the process, so cleanup remains possible. Keep machine output
separate from diagnostics and check all command exit codes.

Image deployment uses local Git, SSH and rsync; the target needs rootless
Podman/systemd, rsync, `flock` and `jq`. Registry authentication uses the SSH user's
existing Podman configuration. Report missing prerequisites before rollout;
do not install packages implicitly.

## Release transaction

Validate explicitly listed artifacts and stage them under
`<target_dir>/.deploy/image/staging/<release_id>`. Do not invent default Go
artifacts in image mode. Finalize only the internal image placeholder after
validating the pulled image ID.

An existing Quadlet without confirmed image-deployment ownership is not adopted
implicitly. Switching an existing binary service to image mode requires an
explicit migration or a new service name.

1. Acquire an exclusive lock per SSH user and service, even across different
   target directories. Hold it throughout inspection, pull, activation and recovery.
2. Capture the previous Quadlet bytes, activity, exact image, managed environment
   file and affected artifacts. Compare existing deployments with their manifests.
3. Preserve the old image with a retention tag and finish the rollback backup.
   Record explicitly when there is no predecessor.
4. Pull the configured image. Pull, authentication, platform or inspection failure
   leaves the active service unchanged. Use that pull's ID, not a later tag lookup.
5. Verify image ID, OS and architecture; pin the candidate and retain its image.
6. Run the target Quadlet generator against an isolated candidate unit directory,
   including required network definitions. Fail before activation on invalid units.
7. Persist the pending manifest, then install managed files and atomically replace
   the active Quadlet. Reload systemd and start/restart the service. If configured,
   `stop_on_deploy` takes effect only at this activation boundary.
8. Check service activity, the actual container image ID and every configured
   health check within the deadline. An HTTP success with the wrong image fails.
9. Atomically commit current and predecessor release IDs in one `current.json`,
   then clear pending state and report the confirmed release/image.

Failures before activation do not restart the old service. This is a controlled
restart of one service, with possible interruption.

## Retained state and interruption

```text
<target_dir>/.deploy/image/
  current.json
  pending.json
  releases/<release_id>/
    manifest.json
    candidate.container
    before/
  staging/<release_id>/
```

Manifests record schema, release/Git identity, service, configured reference,
actual image ID, available registry digests, retention tag, timestamp, check
result and predecessor presence. Backups retain managed file paths/content and
original activity. Never copy environment secret values into manifests or logs.
Use private backup directories and environment files, and atomic same-filesystem
replacement for records.

Protect current, previous and pending images from prune with local tags.
Cleanup occurs only after confirmed release and cannot remove manifest-referenced
images. A hard process/host interruption leaves pending state. The next mutation
resolves it under the same lock: finish an already committed candidate only when
its identity and readiness agree; otherwise restore the predecessor and old
commit record, or undo an incomplete first installation.

## Rollback contract

| Failure | Required result |
| --- | --- |
| Invalid input, missing artifact or occupied lock | Stop without changing service activity |
| Pull or candidate validation fails | Retain active files and service |
| Activation/readiness fails with a predecessor | Restore exact Quadlet, managed files, image and original activity; verify the predecessor |
| First installation fails after activation | Stop the new service, remove only newly created managed files/Quadlet, reload systemd |
| Rollback fails | Report both errors, exit unsuccessfully, retain backups and pending state |
| Registry tag has changed | Restore the retained local image without pulling or rebuilding |

Restore the previous Quadlet byte-for-byte, including network, startup, resource
and health settings. Previously inactive services remain inactive. Restore only
files managed by this transaction; preserve unrelated files and mounted data.
Record existing, changed and new paths separately. Reject paths escaping the
project boundary or overlapping `.deploy`, including unsafe symlink resolution.

The original file/image rollback contract does not undo arbitrary application
migrations. Later explicit database snapshot/rewind support and its constraints
are described in the [application guide](../applications.md#transaction-state-and-rollback).

## CLI and dry run

Use `deploy init --mode=image` for an image template and the existing
`deploy release prod` or `deploy release v1.2.3 prod` forms. Default templates
have no router, published port or mandatory environment file. Do not overwrite
existing project configuration.

Dry run describes normalized defaults, pull, backup, activation, health and
rollback without SSH, registry access, Git push, builds, rsync, service actions
or candidate files. Display unresolved runtime IDs symbolically. Verbose output
must also omit credentials and environment-file contents.

Status exposes configured reference, confirmed image and pending state. Status
and logs retain existing service/container addressing. Mutating service commands
use the same lock as releases.

<a id="abnahmekriterien"></a>

## Original acceptance criteria

These ten entries retain the original D-A01–D-A10 order:

1. A configuration-only Git project deploys without Go source, binary or Dockerfile.
2. An omitted mode preserves the binary workflow.
3. Releases start the selected image; later tag changes cannot alter confirmed releases.
4. Internal services work without Traefik service dependency, router, host port or mandatory `.env`.
5. Health checks run in their documented context and bound rollout duration.
6. Rollback restores the image and all transaction-managed configuration, including first-install and previously inactive cases.
7. Interrupted work remains identifiable and can be recovered before the next release.
8. Dry run and pre-activation errors leave running services unchanged.
9. Current and previous images survive prune.
10. Existing binary tests, failure scenarios and rootless Quadlet/Gotenberg integration checks pass.

Live qualification is still separate from implementation of these checks.

## Deliberate limits

No arbitrary Dockerfile build mode, multi-service orchestration, automatic network
creation, new registry-credential flow or uninterrupted blue-green deployment is
part of this contract. Those would need their own design and acceptance work.
