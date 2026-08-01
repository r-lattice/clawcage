#!/usr/bin/env bash
# kernel.sh — fetch the pinned Firecracker-compatible vmlinux. Build box only.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=build/pins.env
source build/pins.env
[[ -n "$KERNEL_URL" && -n "$KERNEL_SHA256" ]] || {
  echo "kernel.sh: KERNEL_URL / KERNEL_SHA256 not pinned in build/pins.env — resolve and pin first" >&2
  exit 1
}
mkdir -p kernel
curl -fSLo kernel/vmlinux "$KERNEL_URL"
echo "${KERNEL_SHA256}  kernel/vmlinux" | sha256sum -c -
echo "kernel.sh: kernel/vmlinux fetched and checksum-verified"
