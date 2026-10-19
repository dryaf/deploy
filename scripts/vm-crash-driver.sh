#!/usr/bin/env bash
# Sourced by the isolated harness; all images, disks and credentials are disposable.
set -euo pipefail
[[ $test_root != *[:,]* ]] || { echo "VM fixture paths must not contain colon or comma" >&2; exit 1; }
container_tool exec -i "$container_name" bash -s <<'VM_BOOTSTRAP'
set -euo pipefail
dnf install -y --setopt=install_weak_deps=False kernel-core kernel-modules-core dracut e2fsprogs systemd-networkd systemd-resolved
kernel_version=$(rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n' kernel-core | sort -V | tail -1)
kernel="/usr/lib/modules/$kernel_version/vmlinuz"
[[ -f $kernel ]] || kernel="/boot/vmlinuz-$kernel_version"
cp -- "$kernel" /var/tmp/deploy-vm.vmlinuz
dracut --force --no-hostonly --kver "$kernel_version" --add-drivers 'virtio_pci virtio_blk virtio_net ext4' /var/tmp/deploy-vm.initrd
install -d /etc/systemd/network
printf '[Match]\nName=en* eth*\n[Network]\nDHCP=yes\n' > /etc/systemd/network/20-deploy-vm.network
systemctl enable sshd systemd-networkd systemd-resolved
VM_BOOTSTRAP
container_tool cp "$container_name:/var/tmp/deploy-vm.vmlinuz" "$test_root/vmlinuz"
container_tool cp "$container_name:/var/tmp/deploy-vm.initrd" "$test_root/initrd"
container_tool cp "$container_name:/home/podman/.ssh/deploy-integration" "$test_root/vm-key"
container_tool cp "$container_name:/etc/ssh/ssh_host_ed25519_key.pub" "$test_root/vm-host-key.pub"
chmod 600 "$test_root/vm-key"
container_tool export --output "$test_root/rootfs.tar" "$container_name"
builder_image="localhost/deploy-vm-builder:$$"
container_tool commit "$container_name" "$builder_image"
container_tool run --rm --entrypoint=/bin/bash -v "$test_root:/vm:Z" -- "$builder_image" -c '
set -euo pipefail
mkdir -m 700 /vm/rootfs
tar -xpf /vm/rootfs.tar --exclude="dev/*" --exclude="proc/*" --exclude="sys/*" --exclude="run/*" -C /vm/rootfs
rm -f /vm/rootfs/etc/resolv.conf
ln -s /run/systemd/resolve/resolv.conf /vm/rootfs/etc/resolv.conf
printf "/dev/vda / ext4 defaults 0 0\n" > /vm/rootfs/etc/fstab
# The base image uses file capabilities for these helpers; export does not retain
# those capabilities. A native VM uses conventional root-owned setuid helpers.
chown 0:0 /vm/rootfs/usr/bin/newuidmap /vm/rootfs/usr/bin/newgidmap
chmod 4755 /vm/rootfs/usr/bin/newuidmap /vm/rootfs/usr/bin/newgidmap
printf "podman:100000:65536\n" > /vm/rootfs/etc/subuid
printf "podman:100000:65536\n" > /vm/rootfs/etc/subgid
mkdir -p /vm/rootfs/home/podman/.config/containers
printf "[storage]\ndriver=\"vfs\"\n" > /vm/rootfs/home/podman/.config/containers/storage.conf
chown -R 1000:1000 /vm/rootfs/home/podman/.config
# Drop settings intended only for running Podman inside another container.
rm -f /vm/rootfs/etc/containers/containers.conf /vm/rootfs/home/podman/.config/containers/containers.conf

truncate -s 12G /vm/disk.raw
mkfs.ext4 -q -F -E lazy_itable_init=0,lazy_journal_init=0 -d /vm/rootfs /vm/disk.raw
chmod 600 /vm/disk.raw
rm -rf /vm/rootfs
'
rm -- "$test_root/rootfs.tar"
vm_port=$(python3 - <<'PY'
import socket
with socket.socket() as connection:
    connection.bind(('127.0.0.1', 0))
    print(connection.getsockname()[1])
PY
)
printf '[127.0.0.1]:%s ' "$vm_port" > "$test_root/vm-known-hosts"
cut -d' ' -f1-2 "$test_root/vm-host-key.pub" >> "$test_root/vm-known-hosts"
vm_ssh_args=(-F /dev/null -i "$test_root/vm-key" -p "$vm_port" -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=2 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$test_root/vm-known-hosts")
vm_ssh() { ssh "${vm_ssh_args[@]}" podman@127.0.0.1 "$@"; }
vm_start() {
 local -a acceleration=(-accel tcg -cpu max)
 if [[ -r /dev/kvm && -w /dev/kvm ]]; then acceleration=(-accel kvm -cpu host); fi
 printf 'Booting disposable VM with %s\n' "${acceleration[*]}"
 qemu-system-x86_64 "${acceleration[@]}" -smp 2 -m 2048 -display none -monitor none -serial "file:$test_root/vm-serial.log" -no-reboot \
  -kernel "$test_root/vmlinuz" -initrd "$test_root/initrd" -append 'root=/dev/vda rw console=ttyS0 selinux=0 systemd.gpt_auto=0' \
  -drive "file=$test_root/disk.raw,format=raw,if=virtio,cache=none,aio=native" \
  -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$vm_port-:22" -device virtio-net-pci,netdev=net0 > "$test_root/qemu.log" 2>&1 &
 vm_pid=$!
 local attempt
 for attempt in {1..300}; do
  if ! kill -0 "$vm_pid" 2>/dev/null; then cat "$test_root/qemu.log"; tail -60 "$test_root/vm-serial.log"; return 1; fi
  if vm_ssh 'test -S /run/user/1000/bus' >/dev/null 2>&1; then return 0; fi
  sleep 2
 done
 echo 'VM boot/SSH deadline exceeded' >&2
 tail -60 "$test_root/vm-serial.log" >&2
 return 1
}
vm_power_cut() {
 kill -KILL "$vm_pid"
 wait "$vm_pid" 2>/dev/null || true
 vm_pid=''
}
vm_test() {
 local test_name=$1
 vm_ssh "env HOME=/home/podman XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus DEPLOY_IMAGE_INTEGRATION=isolated DEPLOY_VM_CRASH_TEST=isolated DEPLOY_VM_MODE=$vm_mode DEPLOY_VM_PHASE=$vm_phase DEPLOY_TEST_SSH_PORT=22 DEPLOY_TEST_SSH_KEY=/home/podman/.ssh/deploy-integration DEPLOY_TEST_SSH_KNOWN_HOSTS=/home/podman/.ssh/deploy-known-hosts /usr/local/bin/deploy-integration.test -test.v -test.run '^$test_name\$' -test.timeout=10m"
}
vm_start
for vm_mode in binary image; do
 for vm_phase in pending database-snapshot candidate-activation database-restoring commit pending-cleared; do
  scenario="$vm_mode-$vm_phase"
  printf 'VM crash scenario: %s\n' "$scenario"
  vm_ssh 'rm -f /home/podman/deploy-vm-ready.json'
  vm_test TestVMCrashPrepare > "$test_root/prepare-$scenario.log" 2>&1 & prepare_pid=$!
  checkpoint=false
  for attempt in {1..180}; do
   if ! kill -0 "$prepare_pid" 2>/dev/null; then cat "$test_root/prepare-$scenario.log"; echo 'Candidate exited before checkpoint' >&2; exit 1; fi
   if vm_ssh "jq -e --arg expected '$scenario' '.scenario == \$expected' /home/podman/deploy-vm-ready.json" >/dev/null 2>&1; then checkpoint=true; break; fi
   sleep 1
  done
  $checkpoint || { cat "$test_root/prepare-$scenario.log"; echo 'VM checkpoint deadline exceeded' >&2; exit 1; }
  vm_power_cut
  wait "$prepare_pid" 2>/dev/null || true
  vm_start
  vm_test TestVMCrashRecover
 done
done
vm_power_cut
printf 'VM power interruption matrix passed for both deployment modes.\n'
