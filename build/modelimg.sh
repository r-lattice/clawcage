#!/usr/bin/env bash
# modelimg.sh — pack models/<model> into models.ext4 as /model.gguf.
# Usage: build/modelimg.sh <model-filename-under-models/>
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=build/pins.env
source build/pins.env
# The pin gate, same shape as kernel.sh/rootfs.sh: an empty pin is a refusal, never a
# skipped check. Otherwise a bundle could be built around an unpinned model by deleting
# one line from pins.env.
[[ -n "$MODEL_SHA256" ]] || {
  echo "modelimg.sh: MODEL_SHA256 not pinned in build/pins.env — resolve and pin the model first" >&2
  exit 1
}
model="models/${1:?usage: modelimg.sh <model-file>}"
[[ -f "$model" ]] || { echo "modelimg.sh: $model not found" >&2; exit 1; }
# Re-verify the bytes we are about to bake in. Whoever downloaded models/<file> checked
# it at fetch time; this script is a separate invocation, possibly a separate day, and
# what it packs is whatever sits at that path NOW — a truncated download, a hand-swapped
# GGUF, or bit-rot would otherwise be laminated into models.ext4 unnoticed and shipped
# as the pinned model. The pin is only worth what re-checking it is worth.
echo "${MODEL_SHA256}  ${model}" | sha256sum -c - || {
  echo "modelimg.sh: $model does not match MODEL_SHA256 in build/pins.env — refusing to pack it" >&2
  exit 1
}
size_mb=$((($(stat -c%s "$model") / 1048576) + 256))
# Disk-backed, not tmpfs: mktemp -d defaults to /tmp, which on Fedora is RAM, and the
# staging copy below is the whole model — 1.2 GB for the current pin. Override with
# SKIFF_BUILD_TMP (must be disk-backed).
work=$(mktemp -d "${SKIFF_BUILD_TMP:-$PWD/.skiff-build}.XXXXXX")
trap 'rm -rf "$work"' EXIT
# Unprivileged assembly — see the note in rootfs.sh step 4. `mkfs.ext4 -d` populates
# the image from a staging tree, so no `sudo mount -o loop` is needed. The model is
# staged under its in-guest name because the guest init opens /models/model.gguf.
mkdir -p "$work/stage"
cp "$model" "$work/stage/model.gguf"
dd if=/dev/zero of=models.ext4 bs=1M count="$size_mb"
mkfs.ext4 -q -F -d "$work/stage" models.ext4
echo "modelimg.sh: models.ext4 built (${size_mb} MiB) from $model"
