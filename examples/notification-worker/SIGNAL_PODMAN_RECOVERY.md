# Rootless Podman Signal recovery handoff

`recovery-signal-podman-handoff` connects an existing **interactive** Signal
Quadlet to the account fence and offline inbox installation. The stopped gateway
must already have a completed recovery publication. The command fences the
account before stopping its enrolled relay, verifies that account consumers have
exited, and installs the exact retained inbox cursor into a separate directory.
It leaves account runtime disabled and starts no receiver.

This is a separate backend from `recovery-signal-handoff`, which supports a native
host service. Neither command writes/loads a unit, changes a socket/configuration,
starts a verification server or grants production admission. The legacy
`deploy-backup-signal.service` relay has a different configuration and remains
outside this adapter; its ownership/cutover requires the remaining backup work.

## Supported existing installation

The first supported profile is the
[example Quadlet](signal-recovery.container.example), also rendered by
`RenderPodmanSignalRecoveryQuadlet`. The source must match the rendered profile
exactly after substituting enrolled paths, image ID and network. Recovery does
not create this installation or convert arbitrary existing units into it.

- Local rootless Podman, cgroup v2 and a native Linux AMD64/ARM64 image. All Podman
  calls explicitly select `--remote=false`. The image must already exist;
  recovery never pulls it. `image_id` is the full 64-character **local image
  configuration ID**, without `sha256:`, not a registry manifest digest or tag.
- Fixed unit/container names `deploy-notification-signal.service` and
  `deploy-notification-signal`. The Quadlet is in the current user's
  `$XDG_CONFIG_HOME/containers/systemd` (default `~/.config/containers/systemd`).
  Its loaded fragment is in that user's runtime `systemd/generator`,
  `generator.early` or `generator.late` directory. Both files are hash-pinned.
- The generated command profile is tested against Podman/Quadlet 5.7.0.
  [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
  may reorder independent options, so the validator compares complete permitted
  option/value pairs, rejects duplicates/extra arguments, and separately checks
  the typed systemd start, stop and cleanup commands. Other generator layouts
  fail closed. No stale reload, alias, drop-in or pending manager job is accepted.
- The container runs `/usr/local/bin/deploy notification-worker __signal-relay
  --local --config /relay-config.json` as container UID 0 in the default rootless
  user namespace. Read-only rootfs, no added/effective/bounding capabilities,
  no-new-privileges, private PID namespace, no devices and no container restart
  policy are mandatory. The unit owns restart and shutdown.
- Six exact bind mounts expose the account, inbox, socket directory, relay JSON
  and two private credential files. Extra, duplicate, renamed or differently
  writable mounts are refused. Configuration/credentials are read-only. All host
  inputs/state directories must be disjoint, canonical and free of symlinks.
  This initial profile excludes spaces, quotes, backslashes, colons and systemd
  expansion characters in paths. `network` is `pasta`, or `none` for offline
  fixtures; the capability probe always uses `none` independently of this setting.

The relay JSON uses **container paths**: `state_dir` `/signal-state`, `socket`
`/relay/signal.sock`, `account_file` `/secrets/signal-account`, `recipient_file`
`/secrets/signal-recipient`, and interactive `inbox_dir` `/signal-inbox`.
The interactive profile still requires its pinned CLI version, immutable account,
destinations and actors. The gateway's Signal account must match, and its socket
must be the host socket directory plus `signal.sock`.

## Request and retry

Copy [signal-recovery-podman.json](signal-recovery-podman.json) to a mode-0600
private file. Replace every placeholder, including the actual UID in the fragment
path. Use one new operation ID (`n-` and 32 lowercase hexadecimal digits) and keep
it for exact retries. Pin the existing container's full ID before it is removed
by a successful service stop. Hash the exact gateway/relay JSON, Quadlet, generated
unit and deploy executable inside the image. `commit_hash` comes from the completed
gateway recovery publication.

```sh
deploy --dry-run notification-worker recovery-signal-podman-handoff --local \
  --spec /absolute/private/signal-recovery-podman.json
deploy notification-worker recovery-signal-podman-handoff --local \
  --spec /absolute/private/signal-recovery-podman.json
```

Dry-run reads only pinned local inputs. It opens no recovery database/lease and
runs no manager, Podman command or binary probe. During execution, keep the
installation unchanged and stop any independently managed account consumers.
The operation retains gateway/account-operation leases and rechecks its bindings
around external work. The immutable service binding also includes credentials
and Podman's graph/run roots.

For an active container, metadata must match the enrolled ID, image, commands,
isolation and mounts, with conmon matching systemd's MainPID. Its retained creation
command must also match the allowed Quadlet options; extra environment or other
creation overrides are refused. An additional
read-only helper runs under `podman unshare`, using the installer's already-open
executable descriptor. It checks the actual process executable hash/inode, argv,
UID/start identity, namespace-visible mount inodes and account/inbox flock
descriptors. Replacing a bind-mounted host file with identical bytes still fails
this inode check. The helper does not execute inside the target container.

The image is independently probed in a disposable container with no account or
configuration mounts, no network and no image-declared volumes. The new
`__signal-podman-recovery-probe` returns the recovery protocol version **and its
own executable hash**, so the executable pin is checked even after the enrolled
container has been removed. Older images without this probe are refused before
fencing or stopping. Probe/manager stderr and credential contents are not emitted
in operational errors.

After interruption, re-run the identical request. A lost stop response is safe to
retry: a removed original container plus a stopped unit is accepted, while a
replacement by name or a renamed original container is rejected. Saved results
are independently verified and an already stopped service is not stopped again.
The response contains `installation` and `account`; completion still reports
`runtime_allowed: false`. Retain all account, source and destination evidence.
There is no fence-clear or receiver-restart step in this command.

## Validation scope

Unit/race tests exercise command, image, container, mount, credential, manager and
store drift; missing authority fields; capability rejection; interrupted stop;
and exact replay. A real generator test checks the supported command layout.

The opt-in `TestPodmanSignalHandoffIsolatedContainer` builds a local scratch image
and uses real rootless Podman in a separate temporary VFS store. A synthetic
relay holds real account/inbox flocks; the built deploy CLI performs the recovery.
It checks a replaced bind-mount inode, lost stop response, permanent fencing and
replay without another stop. Containers have `network=none`; systemctl/busctl are
strict fixture programs. No real user-systemd unit, Signal account or provider is
used. Run it with `DEPLOY_PODMAN_FIXTURE=1` and the `notificationintegration` tag.
Native ARM64 and live-service/provider qualification remain outstanding.
