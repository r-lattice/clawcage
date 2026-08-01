#!/usr/bin/env bash
# rootfs.sh — build rootfs/rootfs.ext4: busybox + static llama-server + init.
# Build box only. Needs: git, cmake, gcc-c++, e2fsprogs, file. No sudo: the image is
# assembled unprivileged with `mkfs.ext4 -d` (see step 4).
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=build/pins.env
source build/pins.env
[[ -n "$BUSYBOX_URL" && -n "$BUSYBOX_SHA256" ]] || {
  echo "rootfs.sh: busybox not pinned in build/pins.env" >&2
  exit 1
}
[[ "$LLAMA_CPP_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
  echo "rootfs.sh: LLAMA_CPP_COMMIT must be a full 40-char commit sha, not a tag (X-2) — got '${LLAMA_CPP_COMMIT}'" >&2
  exit 1
}
# `file` is a hard prerequisite, demanded HERE rather than at the guard that uses it
# (step 1, below the build) for two reasons. First, correctness: that guard is
# `if file "$bin" | grep -q "dynamically linked"`, and a missing or erroring `file`
# leaves the pipeline's status as grep's "no match" (1) — indistinguishable from a
# verified-static binary, so the guard would fail OPEN and ship an image that panics
# at guest boot. Second, cost: discovering the missing tool after a full llama.cpp
# static build wastes the whole build.
command -v file >/dev/null || {
  echo "rootfs.sh: 'file' is required to verify static linking — install it (dnf install file) and re-run" >&2
  exit 1
}
# mktemp -d follows $TMPDIR, defaulting to /tmp — which on Fedora is tmpfs, i.e. RAM.
# The llama.cpp tree plus -j"$(nproc)" worth of C++ objects runs to several GB, and a
# static link is memory-hungry on top of that; on a 16 GB box that is an OOM/freeze
# risk, not a theoretical one. Build on disk beside the repo instead. Override with
# SKIFF_BUILD_TMP to put the scratch tree somewhere else (must be disk-backed).
work=$(mktemp -d "${SKIFF_BUILD_TMP:-$PWD/.skiff-build}.XXXXXX")
trap 'rm -rf "$work"' EXIT

# 1. static llama-server, at an IMMUTABLE commit (X-2). `--branch <tag>` trusts a
# mutable ref: upstream can move or delete a tag and the clone would still succeed.
# Fetching the sha directly means the object we build is the object we pinned, and
# the rev-parse below fails loud if it ever is not.
git init -q "$work/llama.cpp"
git -C "$work/llama.cpp" remote add origin https://github.com/ggml-org/llama.cpp
git -C "$work/llama.cpp" fetch --depth 1 origin "$LLAMA_CPP_COMMIT"
git -C "$work/llama.cpp" checkout --detach "$LLAMA_CPP_COMMIT"
[[ "$(git -C "$work/llama.cpp" rev-parse HEAD)" == "$LLAMA_CPP_COMMIT" ]] || {
  echo "rootfs.sh: checked-out HEAD is not the pinned commit — refusing to build" >&2
  exit 1
}
# BUILD_SHARED_LIBS=OFF only makes llama.cpp's OWN libs static — it still links the
# binary against libc/libstdc++/libgomp/libssl and the dynamic loader. The rootfs below
# is busybox-only: no /lib64/ld-linux-x86-64.so.2, no libc. A dynamically linked
# llama-server exec-fails in the guest and takes init with it (init exec's it, so the
# kernel panics).
#
# CMAKE_EXE_LINKER_FLAGS=-static is the flag that actually does it, and it is NOT
# redundant with GGML_STATIC: GGML_STATIC calls add_link_options(-static) from
# ggml/src/CMakeLists.txt, which only applies to targets in that directory and below.
# llama-server is built under tools/server — a sibling scope — so it never sees the
# flag. Verified: GGML_STATIC alone still produced a dynamically linked binary.
# GGML_STATIC is kept because it also prefers .a over .so when resolving deps.
# OPENMP and OPENSSL are off because each drags in a shared dependency -static cannot
# satisfy here; ggml falls back to its own pthread threadpool.
cmake -S "$work/llama.cpp" -B "$work/build" \
  -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF -DLLAMA_CURL=OFF -DCMAKE_BUILD_TYPE=Release \
  -DGGML_STATIC=ON -DGGML_OPENMP=OFF -DLLAMA_OPENSSL=OFF \
  -DCMAKE_EXE_LINKER_FLAGS=-static
cmake --build "$work/build" --target llama-server -j"$(nproc)"

# Fail loud rather than discover this at guest-boot time, where the only symptom is a
# kernel panic with no useful message. This guard is the whole reason the flags above
# are what they are. `file`'s presence is enforced at the top of this script — without
# that gate this `if` reads "no match" when `file` is absent and passes silently.
if file "$work/build/bin/llama-server" | grep -q "dynamically linked"; then
  echo "rootfs.sh: llama-server came out dynamically linked — the busybox-only rootfs has no loader and no libc, so this image would panic at boot. Refusing to build." >&2
  file "$work/build/bin/llama-server" >&2
  exit 1
fi

# 2. static busybox
curl -fSLo "$work/busybox" "$BUSYBOX_URL"
echo "${BUSYBOX_SHA256}  ${work}/busybox" | sha256sum -c -
chmod +x "$work/busybox"

# 3. guest init — adversary-realistic probe (P-1/P-2). The old version measured the
#    guest's OWN routing table (empty by default → ENETUNREACH → "BLOCKED"), and every
#    machinery failure — nc missing from the busybox build, init dying early, the
#    models mount failing — landed in the same reassuring branch. It failed OPEN.
#    The rewrite does what TA-1 would do: install a default route first, then probe
#    TCP and UDP, and report machinery failure as its own third state.
cat > "$work/init" <<'INIT'
#!/bin/busybox sh
# skiff guest init.
#
# EVIDENCE CONTRACT with ./proof — exactly ONE of these three lines is ever printed:
#   SKIFF-OUTBOUND-ESCAPED   a probe reached the outside world. Isolation is broken.
#   SKIFF-PROBE-BROKEN: <r>  the evidence machinery itself failed. NOT "blocked".
#   SKIFF-OUTBOUND-BLOCKED   both probes ran, neither connected.
# These lines are guest-authored: under the threat model the guest is the adversary,
# so they are corroboration only. The authoritative evidence is the host's nftables
# drop counters, which the guest cannot write (security review P-1).
BB=/bin/busybox

say() { echo "$*" > /dev/console; }
serve() { exec /bin/llama-server -m /models/model.gguf --host 0.0.0.0 --port 8080; }
# Any failure of the machinery is BROKEN, never BLOCKED — "could not probe" must
# never be confusable with "was prevented".
broken() { say "SKIFF-PROBE-BROKEN: $*"; serve; }

"$BB" mount -t proc proc /proc     || broken "mount /proc failed"
"$BB" mount -t sysfs sys /sys      || broken "mount /sys failed"
"$BB" mount -t tmpfs tmpfs /tmp    || broken "mount /tmp failed"
"$BB" mkdir -p /models             || broken "mkdir /models failed"
"$BB" mount -o ro /dev/vdb /models || broken "mount /dev/vdb on /models failed"

# The probe needs these two applets; a busybox build without them would otherwise
# fail silently into the wrong branch (this is exactly the P-2 failure mode).
"$BB" --list | "$BB" grep -qx nc || broken "busybox build has no nc applet"
"$BB" --list | "$BB" grep -qx ip || broken "busybox build has no ip applet"

# 1. Do what the adversary does FIRST: give ourselves a default route. Addressing is
#    a fixed /30 per instance — guest 172.30.N.2, host peer 172.30.N.1 — so the peer
#    is derivable from our own address with no boot-arg plumbing.
guest_ip=$("$BB" ip -4 addr show eth0 | "$BB" sed -n 's#.*inet \([0-9.]*\)/.*#\1#p' | "$BB" head -n 1)
[ -n "$guest_ip" ] || broken "no IPv4 address on eth0 — cannot derive the /30 peer"
gw=$(echo "$guest_ip" | "$BB" sed 's/\.2$/.1/')
[ "$gw" != "$guest_ip" ] || broken "guest address $guest_ip is not the .2 of a skiff /30"
#    `replace`, not `add`: in NORMAL mode the ip= boot arg leaves the gateway field
#    empty, so no default route exists and we install one. In leak-demo mode the
#    launcher puts the host in that field, so the KERNEL has already installed an
#    identical default route — `add` then fails EEXIST, trips broken(), and the
#    escape is never attempted, masking the very tripwire the demo exists to prove.
#    `replace` is idempotent in both modes; the || broken guard stays, so a genuine
#    routing failure is still reported BROKEN rather than mistaken for BLOCKED.
"$BB" ip route replace default via "$gw" || broken "ip route replace default via $gw failed"

# 2. Probe TCP and UDP. TCP: a completed connect is an escape. UDP is connectionless,
#    so a zero exit status proves nothing — a real reply is the only signal, hence the
#    hand-built DNS query for a.root-servers.net and the byte count of the answer.
"$BB" nc -w 2 1.1.1.1 443 </dev/null >/dev/null 2>&1
tcp_rc=$?
udp_bytes=$("$BB" printf '\125\125\001\000\000\001\000\000\000\000\000\000\001a\014root-servers\003net\000\000\001\000\001' \
  | "$BB" nc -u -w 2 1.1.1.1 53 2>/dev/null | "$BB" wc -c | "$BB" tr -d ' ')
case "$udp_bytes" in
  '' | *[!0-9]*) broken "UDP probe produced no byte count ('$udp_bytes')" ;;
esac

# 3. One if/elif/else: ESCAPED is decided before BLOCKED can be reached, and the two
#    lines are structurally incapable of both appearing.
if [ "$tcp_rc" -eq 0 ]; then
  say "SKIFF-OUTBOUND-ESCAPED: TCP connect to 1.1.1.1:443 succeeded via $gw — ISOLATION BROKEN"
elif [ "$udp_bytes" -gt 0 ]; then
  say "SKIFF-OUTBOUND-ESCAPED: UDP 1.1.1.1:53 answered with ${udp_bytes} bytes via $gw — ISOLATION BROKEN"
else
  say "SKIFF-OUTBOUND-BLOCKED: default route via $gw installed, TCP 443 rc=${tcp_rc}, UDP 53 reply bytes=0 — neither probe connected"
fi

# Restore the as-shipped posture: the route existed only for the probe.
"$BB" ip route del default via "$gw" 2>/dev/null
serve
INIT

# 4. assemble ext4 — unprivileged. The original recipe was
# `mkfs.ext4` + `sudo mount -o loop` + cp + umount; a loop mount needs root, which a
# non-interactive build box may not have. `mkfs.ext4 -d <dir>` (e2fsprogs) populates
# the filesystem straight from a staging tree with no mount and no privileges, and
# preserves modes — the exec bits below survive into the image. Layout on disk
# differs from the mounted-and-copied version; the file tree it presents does not.
img=rootfs/rootfs.ext4
mkdir -p rootfs
stage="$work/stage"
mkdir -p "$stage/bin" "$stage/proc" "$stage/sys" "$stage/tmp" "$stage/dev" "$stage/models"
cp "$work/busybox" "$stage/bin/busybox"
cp "$work/build/bin/llama-server" "$stage/bin/llama-server"
cp "$work/init" "$stage/init"
chmod +x "$stage/init" "$stage/bin/busybox" "$stage/bin/llama-server"
dd if=/dev/zero of="$img" bs=1M count=512
mkfs.ext4 -q -F -d "$stage" "$img"
echo "rootfs.sh: rootfs/rootfs.ext4 built ($(du -h "$img" | cut -f1))"
