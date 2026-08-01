#!/usr/bin/env bash
# bundle.sh — assemble the portable stick/ layout from built artifacts.
set -euo pipefail
# X-2: never let the Go toolchain fetch itself over the network mid-build.
export GOTOOLCHAIN=local
cd "$(dirname "$0")/.."
# LICENSE travels with the bundle: the stick IS the distribution, and Apache-2.0 §4(a) wants a
# copy of the licence to go with it. The README on the stick also points at it.
for f in run netsetup proof bin/firecracker kernel/vmlinux rootfs/rootfs.ext4 models.ext4 config.yaml README.md LICENSE; do
  [[ -e "$f" ]] || { echo "bundle.sh: missing $f — build it first" >&2; exit 1; }
done
rm -rf stick
mkdir -p stick/bin stick/kernel stick/rootfs stick/models
cp run netsetup proof config.yaml README.md LICENSE stick/
cp bin/firecracker stick/bin/
cp kernel/vmlinux stick/kernel/
cp rootfs/rootfs.ext4 stick/rootfs/
cp models/*.gguf stick/models/
cp models.ext4 stick/
# X-1: the pins protect the BUILD BOX; this protects the artifact in the field.
# Without a manifest, a stick whose rootfs.ext4 was replaced in transit runs cleanly
# and `proof` still passes — every host-side check measures the HOST's configuration,
# not the BUNDLE's contents. `run up` verifies this file before booting anything.
# Excluded from its own listing; sorted by path so two builds' manifests are diffable.
# config.yaml is excluded too: it is the ONE file the README tells the operator to edit, so
# listing it would make the documented workflow report TAMPERED (unlisted = warning, not fatal).
# Built into a temp file OUTSIDE stick/ and moved in: a redirect straight into
# stick/MANIFEST.sha256 creates the file before `find` walks the tree — which is
# exactly what SC2094 warns about.
manifest=$(mktemp)
(cd stick && find . -type f ! -name MANIFEST.sha256 ! -path ./config.yaml -exec sha256sum {} + | sort -k2) > "$manifest"
mv "$manifest" stick/MANIFEST.sha256
chmod 0644 stick/MANIFEST.sha256
echo "bundle.sh: stick/ assembled — $(du -sh stick | cut -f1) total, $(wc -l < stick/MANIFEST.sha256) file(s) in MANIFEST.sha256"
