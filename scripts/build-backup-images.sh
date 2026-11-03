#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${BACKUP_DEBIAN_IMAGE:?Set the verified Debian base image digest}"
: "${BACKUP_DEBIAN_SNAPSHOT:?Set an immutable Debian snapshot timestamp}"
: "${BACKUP_IMAGE_NAME:?Set the local manifest name to build}"
[[ "$BACKUP_DEBIAN_IMAGE" =~ @sha256:[a-f0-9]{64}$ ]] || exit 1
[[ "$BACKUP_DEBIAN_SNAPSHOT" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || exit 1
(cd dist/backup-worker && sha256sum --check SHA256SUMS)
context=$(mktemp -d "${TMPDIR:-/tmp}/deploy-backup-image.XXXXXXXX")
trap 'rm -rf -- "$context"' EXIT
mkdir -p "$context/dist/backup-worker"
cp dist/backup-worker/deploy-linux-amd64 dist/backup-worker/deploy-linux-arm64 "$context/dist/backup-worker/"
for arch in amd64 arm64; do
 podman build --platform="linux/$arch" --build-arg TARGETARCH="$arch" --build-arg DEBIAN_IMAGE="$BACKUP_DEBIAN_IMAGE" --build-arg DEBIAN_SNAPSHOT="$BACKUP_DEBIAN_SNAPSHOT" --manifest "$BACKUP_IMAGE_NAME" -f packaging/backup-worker/Containerfile "$context"
done
printf '%s\n' 'Local manifest built. Run the qualification/release gates before an explicit registry push.'
