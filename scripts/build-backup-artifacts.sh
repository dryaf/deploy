#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
required_go=$(cat packaging/backup-worker/go-version)
[[ $(go env GOVERSION) == "go${required_go}" ]] || { echo "Use Go ${required_go} for matching reproducible artifacts" >&2; exit 1; }
mkdir -p dist/backup-worker
for arch in amd64 arm64; do
 CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags=-buildid= -o "dist/backup-worker/deploy-linux-$arch" ./cmd/deploy
done
chmod 0755 dist/backup-worker/deploy-linux-amd64 dist/backup-worker/deploy-linux-arm64
(cd dist/backup-worker && sha256sum deploy-linux-amd64 deploy-linux-arm64 > SHA256SUMS)
printf '%s\n' 'Static helper artifacts built. This does not qualify either architecture or publish images.'
