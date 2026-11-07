#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Bundles are local release inputs. Publishing or pushing is a separate action.
./scripts/build-backup-artifacts.sh
if [[ -n "${WORKER_LED_SOURCE:-}" ]]; then
    python3 scripts/build-led-artifact.py "$WORKER_LED_SOURCE" dist/workers/led-daemon.pyz
    export WORKER_LEDS_AMD64="$PWD/dist/workers/led-daemon.pyz"
    export WORKER_LEDS_ARM64="$PWD/dist/workers/led-daemon.pyz"
fi
go run ./scripts/assemble-worker-bundle.go
