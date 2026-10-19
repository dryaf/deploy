#!/usr/bin/env bash
# Builds a disposable bootable VM from the same pinned integration base.
set -euo pipefail
for tool in qemu-system-x86_64 qemu-img ssh; do command -v "$tool" >/dev/null; done
export DEPLOY_TEST_VM_CRASH=1
exec "$(dirname -- "$0")/test-image-deploy-isolated.sh"
