#!/bin/sh
set -eu
memory_mib=${LANE_VM_MEMORY_MIB-3072}
min_memory_mib=512
max_memory_mib=65536
tcg_cache_mib=128
case "$memory_mib" in
  ''|0*|*[!0123456789]*) echo 'LANE_VM_MEMORY_MIB must be an integer from 512 through 65536.' >&2; exit 2 ;;
esac
if [ "${#memory_mib}" -gt "${#max_memory_mib}" ] ||
   [ "$memory_mib" -lt "$min_memory_mib" ] ||
   [ "$memory_mib" -gt "$max_memory_mib" ]; then
  echo 'LANE_VM_MEMORY_MIB must be an integer from 512 through 65536.' >&2
  exit 2
fi
mkdir -p /work
if [ ! -f /work/disk.qcow2 ]; then
  qemu-img create -f qcow2 -F qcow2 -b /opt/ubuntu.img /work/disk.qcow2 24G
  cloud-localds /work/seed.img /work/user-data /work/meta-data
fi
arch=$(cat /opt/guest-arch)
case "$arch" in
  arm64) set -- qemu-system-aarch64 -machine virt -cpu cortex-a72 -bios /usr/share/qemu-efi-aarch64/QEMU_EFI.fd ;;
  amd64) set -- qemu-system-x86_64 -machine q35 -cpu max ;;
  *) echo "Unsupported guest architecture: $arch" >&2; exit 1 ;;
esac
# No host mounts, Docker socket, hardware device, or production credential.
exec "$@" -accel "tcg,thread=multi,tb-size=$tcg_cache_mib" -m "$memory_mib" -smp 2 -nographic \
  -drive file=/work/disk.qcow2,if=virtio,format=qcow2 \
  -drive file=/work/seed.img,if=virtio,format=raw,readonly=on \
  -netdev user,id=net,hostfwd=tcp:0.0.0.0:2222-:22 -device virtio-net-pci,netdev=net
