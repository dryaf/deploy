#!/usr/bin/env bash
# This creates a disposable rootless systemd container; it never reads deploy.yaml.
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "$0")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/deploy-image-integration.XXXXXXXX")
container_name="deploy-image-integration-$$"
base_image=${DEPLOY_TEST_BASE_IMAGE:-quay.io/podman/stable:v5.4@sha256:52536d900c9e7e7cc924bb737eb8c570eeaa3c50f6a3b0b563eb0ec9c48807e4}
if [[ $(uname -m) != x86_64 && -z ${DEPLOY_TEST_GOTENBERG_IMAGE:-} ]]; then
 echo 'Set DEPLOY_TEST_GOTENBERG_IMAGE to a verified native Chromium image with digest on non-amd64 hosts' >&2
 exit 1
fi
gotenberg_image=${DEPLOY_TEST_GOTENBERG_IMAGE:-docker.io/gotenberg/gotenberg:8.38.1-chromium@sha256:63545039126042c5df622675ccb3dd9efd8b5c1faa0ec5265b787737e6d6757e}
container_tool() {
 env XDG_RUNTIME_DIR="$test_root/runtime" podman --remote=false --root "$test_root/storage" --runroot "$test_root/run" --storage-driver vfs "$@"
}
cleanup() {
 local status=$?
 if [[ -n ${vm_pid:-} ]]; then kill -KILL "$vm_pid" 2>/dev/null || true; wait "$vm_pid" 2>/dev/null || true; fi
 trap - EXIT HUP INT TERM
 if (( status != 0 )) && [[ ${DEPLOY_TEST_KEEP_FAILED_ENVIRONMENT:-} == 1 ]]; then
  printf 'Retained failed test environment: %s (container %s)\n' "$test_root" "$container_name" >&2
  exit "$status"
 fi
 container_tool rm -f "$container_name" >/dev/null 2>&1 || true
 if [[ -d $test_root/rootfs ]]; then container_tool unshare rm -rf -- "$test_root/rootfs" >/dev/null 2>&1 || true; fi
 container_tool system reset --force >/dev/null 2>&1 || true
 rm -rf -- "$test_root" || true
 exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -m 700 -- "$test_root/runtime"
[[ $(id -u) != 0 ]] || { echo 'Run the integration harness as a regular Linux user with subordinate UIDs/GIDs' >&2; exit 1; }
command -v podman >/dev/null
command -v go >/dev/null
cd -- "$repo_dir"
CGO_ENABLED=0 go test -tags=imageintegration -c -o "$test_root/integration.test" .
if [[ $base_image == current ]]; then
 container_tool pull -- quay.io/podman/stable:latest
 base_image=$(container_tool image inspect --format '{{index .RepoDigests 0}}' quay.io/podman/stable:latest)
fi
[[ $base_image == *@sha256:* ]] || { echo 'The test base must resolve to an immutable digest' >&2; exit 1; }
printf 'Test base: %s\n' "$base_image"
container_tool pull -- "$base_image"
container_tool run -d --name "$container_name" --privileged --cgroupns=private --systemd=always --entrypoint=/sbin/init -- "$base_image"
container_tool exec -i "$container_name" bash -s <<'BOOTSTRAP'
set -euo pipefail
# podman exec joins the container's root cgroup. Move this helper away from
# that root so cgroup v2 can enable controllers for systemd's child slices.
for attempt in {1..50}; do [[ -d /sys/fs/cgroup/init.scope ]] && break; sleep .1; done
echo $$ > /sys/fs/cgroup/init.scope/cgroup.procs
dnf install -y jq rsync dbus-daemon git-core openssh-clients openssh-server gawk sqlite
ssh-keygen -A
install -d -m 700 -o 1000 -g 1000 /home/podman/.ssh
ssh-keygen -q -t ed25519 -N '' -f /home/podman/.ssh/deploy-integration
cp /home/podman/.ssh/deploy-integration.pub /home/podman/.ssh/authorized_keys
printf '127.0.0.1 ' > /home/podman/.ssh/deploy-known-hosts
cut -d' ' -f1-2 /etc/ssh/ssh_host_ed25519_key.pub >> /home/podman/.ssh/deploy-known-hosts
chown -R 1000:1000 /home/podman/.ssh
chmod 600 /home/podman/.ssh/authorized_keys /home/podman/.ssh/deploy-known-hosts
# Generated credentials exist only inside this disposable container.
usermod -p x podman
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\n' > /etc/ssh/sshd_config.d/00-deploy-integration.conf
systemctl start sshd
# A suid-enabled namespace-local tmpfs permits nested newuidmap even when the
# developer's workspace/tmp filesystem has nosuid. These mounts exist only here.
mkdir -p /opt/deploy-integration-helpers
mount -t tmpfs -o mode=0755,suid tmpfs /opt/deploy-integration-helpers
cp /usr/bin/newuidmap /usr/bin/newgidmap /opt/deploy-integration-helpers/
chmod 4755 /opt/deploy-integration-helpers/newuidmap /opt/deploy-integration-helpers/newgidmap
mount --bind /opt/deploy-integration-helpers/newuidmap /usr/bin/newuidmap
mount --bind /opt/deploy-integration-helpers/newgidmap /usr/bin/newgidmap
systemctl start systemd-logind
loginctl enable-linger podman
systemctl start user@1000.service
systemctl set-property user@1000.service CPUAccounting=yes MemoryAccounting=yes
# Restart after delegation is established; an already running user manager may
# have cached the previously unavailable controllers in this nested environment.
systemctl restart user@1000.service
BOOTSTRAP
container_tool cp "$test_root/integration.test" "$container_name:/usr/local/bin/deploy-integration.test"
if [[ ${DEPLOY_TEST_VM_CRASH:-} == 1 ]]; then
 source "$repo_dir/scripts/vm-crash-driver.sh"
 exit 0
fi
container_tool exec --env HOME=/home/podman --env XDG_RUNTIME_DIR=/run/user/1000 --env DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus --env DEPLOY_IMAGE_INTEGRATION=isolated --env DEPLOY_TEST_REQUIRE_SSH=1 --env DEPLOY_TEST_SSH_PORT=22 --env DEPLOY_TEST_SSH_KEY=/home/podman/.ssh/deploy-integration --env DEPLOY_TEST_SSH_KNOWN_HOSTS=/home/podman/.ssh/deploy-known-hosts --env "DEPLOY_TEST_GOTENBERG_IMAGE=$gotenberg_image" "$container_name" bash -c 'echo $$ > /sys/fs/cgroup/init.scope/cgroup.procs; exec setpriv --reuid=1000 --regid=1000 --init-groups /usr/local/bin/deploy-integration.test -test.v -test.run "^TestRootlessImageDeploymentIntegration$" -test.timeout=15m'
