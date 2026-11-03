#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $(uname -m) == x86_64 ]] || { printf '%s\n' 'This pinned fixture currently qualifies AMD64 only.' >&2; exit 1; }
fixture_dir=$(mktemp -d "${TMPDIR:-/tmp}/deploy-signal-offline.XXXXXXXX")
trap 'rm -rf -- "$fixture_dir"' EXIT
image=$(python3 -c 'import json; print(json.load(open("packaging/notification-worker/signal-cli.lock.json"))["images"]["linux/amd64"])')
[[ "$image" =~ ^ghcr\.io/asamk/signal-cli@sha256:[a-f0-9]{64}$ ]] || exit 1
CGO_ENABLED=0 go build -o "$fixture_dir/lease-check" ./internal/notification/relay/testdata/jvm-lease
CGO_ENABLED=0 go test -c -o "$fixture_dir/account-check" ./internal/notification/signalaccount
# The caller must explicitly pull the pinned image first. No network, account
# credentials or persistent account directories are available to these runs.
podman run --rm --pull=never --network=none --read-only --cap-drop=all \
  --security-opt=no-new-privileges --pids-limit=128 --memory=768m \
  --tmpfs /tmp:rw,size=256m --entrypoint /opt/signal-cli/bin/signal-cli "$image" --version
podman run --rm --pull=never --network=none --read-only --cap-drop=all \
  --security-opt=no-new-privileges --pids-limit=128 --memory=768m \
  --tmpfs /tmp:rw,size=256m --volume "$fixture_dir/lease-check:/lease-check:ro,Z" \
  --entrypoint /lease-check "$image"
podman run --rm --pull=never --network=none --read-only --cap-drop=all \
  --security-opt=no-new-privileges --pids-limit=128 --memory=768m \
  --tmpfs /tmp:rw,size=256m --volume "$fixture_dir/account-check:/account-check:ro,Z" \
  --env DEPLOY_TEST_OFFLINE_SIGNAL_LINK=1 --entrypoint /account-check "$image" \
  -test.run='^TestSignalLinkOfflineCLI$' -test.timeout=30s -test.v
