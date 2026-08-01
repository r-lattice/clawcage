#!/usr/bin/env bash
# test/proof_harness.sh — drive the real ./proof through both directions of every check.
#
# WHY THIS EXISTS. `proof` is the product, and a proof that can only pass proves nothing.
# Every check it makes therefore has to be shown FAILING on a host that deserves to fail,
# not only passing on one that deserves to pass. This harness builds real kernel state —
# a real `inet skiff` nftables table (built by running netsetup ITSELF, so the rules under
# test cannot drift from the ones netsetup ships), real TAP devices, real drop counters
# incremented by real forwarded packets, real console-log files — then runs the real
# ./proof against it and asserts the exit code AND the specific lines it must print.
#
# HOW IT GETS ROOT WITHOUT SUDO. `unshare -rn` runs each scenario as root inside a private
# user + network namespace with a network stack of its own. Nothing here touches the host's
# interfaces, sysctls, nftables ruleset, or the bundle directory: every scenario gets a
# throwaway namespace that dies with it, and every file written lands under one `mktemp -d`.
# The one scenario that cannot run there is the unprivileged-refusal case — it runs first,
# outside the namespace, as the invoking user.
#
# WHAT IS FABRICATED, STATED PLAINLY. There is no Firecracker VM here. The guest's console
# log is written by the harness (it is a guest-authored byte stream in production too — see
# the README on which evidence is authoritative), the health endpoint is a stub HTTP server
# on the guest address, and in the counter scenario the TAP is swapped for a veth pair of
# the same name because a TAP with no VMM attached carries no packets. What is NOT
# fabricated is everything proof grades as authoritative: the nftables table and its rules,
# the kernel's own drop counters, ip_forward, and the per-TAP disable_ipv6 sysctl.
#
# Usage:  bash test/proof_harness.sh     (no sudo, no arguments)
# Exit:   0 = every scenario behaved exactly as asserted; 1 = at least one did not.
set -euo pipefail

self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
repo="$(cd "$(dirname "$self")/.." && pwd)"
proof="$repo/proof"
netsetup_src="$repo/netsetup"

# The three guest markers, verbatim in the shape build/rootfs.sh's init emits them.
blocked_line='SKIFF-OUTBOUND-BLOCKED: default route via 172.30.0.1 installed, TCP 443 rc=1, UDP 53 reply bytes=0 — neither probe connected'
escaped_line='SKIFF-OUTBOUND-ESCAPED: TCP connect to 1.1.1.1:443 succeeded via 172.30.0.1 — ISOLATION BROKEN'
broken_line='SKIFF-PROBE-BROKEN: busybox build has no nc applet'

# =============================================================================
# INNER — one scenario, already inside its own `unshare -rn` namespace.
# =============================================================================
listener_pid=""
guest_pid=""

inner_cleanup() {
  [[ -n "$listener_pid" ]] && kill "$listener_pid" 2>/dev/null
  [[ -n "$guest_pid" ]] && kill "$guest_pid" 2>/dev/null
  return 0
}

# guard_up — raise the real guard by RUNNING NETSETUP. netsetup is copied into the scratch
# dir only because it writes netsetup.state beside itself and the harness never writes into
# the bundle; the script is byte-identical, so the table, the TAP, the addressing and the
# per-TAP sysctls are all netsetup's own, not a re-typed imitation of them.
guard_up() {
  cp "$netsetup_src" "$work/netsetup"
  "$work/netsetup" up 1 "$(id -u)" >/dev/null
}

# lo_up — proof curls the guest health endpoint at 172.30.<n>.2:8080. Most scenarios have no
# guest at all, so that address is carried on lo and answered locally; what is under test is
# whether proof grades reachability, not whether llama-server works.
lo_up() {
  ip link set lo up
  ip addr add 172.30.0.2/32 dev lo
}

write_health_stub() {
  cat >"$work/health_stub.py" <<'PY'
import http.server


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

    def log_message(self, *a):
        pass


http.server.HTTPServer(("0.0.0.0", 8080), H).serve_forever()
PY
}

# health_up [guest_ns_pid] — stand up the stub the guest would serve. With no argument it
# runs here (answering the lo address); with a pid it runs inside that network namespace, so
# proof's curl crosses the real point-to-point link, exactly as it does against a real VM.
health_up() {
  write_health_stub
  if [[ -n "${1:-}" ]]; then
    nsenter -t "$1" -n python3 "$work/health_stub.py" &
  else
    python3 "$work/health_stub.py" &
  fi
  listener_pid=$!
  local _
  for _ in $(seq 1 50); do
    if curl -fsS -m 1 "http://172.30.0.2:8080/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "harness: the stub health endpoint never came up" >&2
  return 1
}

console_log() {
  printf '%s\n' "$@" >"$rundir/skiff-0.console.log"
}

# forwarding_guest_up — build the one topology in which the forward drop counters CAN move.
# A TAP with no VMM attached to its file descriptor is a dead end, so skiff-tap0 is re-created
# as one end of a veth pair (same name, same address, same sysctls — the nft rules match on
# iifname, which is what is under test) and the far end is moved into a nested network
# namespace that plays the guest. The guest address lives only in that namespace: a packet
# whose source is a LOCAL address of the forwarding host is dropped as a martian source before
# it ever reaches the forward hook, so the lo shortcut the other scenarios use cannot be used
# here. proof's health curl then crosses the link for real, which is also what makes the input
# chain's established-accept rule fire.
forwarding_guest_up() {
  ip link set lo up
  ip link del skiff-tap0
  ip link add skiff-tap0 type veth peer name skiff-guest0
  sysctl -qw "net.ipv6.conf.skiff-tap0.disable_ipv6=1"
  ip addr add 172.30.0.1/30 dev skiff-tap0
  ip link set skiff-tap0 up
  # Somewhere for the routing decision to send the packet — off-segment, so it must forward.
  ip link add skiff-out0 type dummy
  ip addr add 10.99.99.1/24 dev skiff-out0
  ip link set skiff-out0 up
  sysctl -qw net.ipv4.ip_forward=1

  unshare -n sleep 60 &
  guest_pid=$!
  local _
  for _ in $(seq 1 50); do
    if [[ "$(readlink "/proc/$guest_pid/ns/net")" != "$(readlink /proc/self/ns/net)" ]]; then
      break
    fi
    sleep 0.1
  done
  ip link set skiff-guest0 netns "$guest_pid"
  nsenter -t "$guest_pid" -n ip link set lo up
  nsenter -t "$guest_pid" -n ip addr add 172.30.0.2/30 dev skiff-guest0
  nsenter -t "$guest_pid" -n ip link set skiff-guest0 up
  nsenter -t "$guest_pid" -n ip route add default via 172.30.0.1
}

# forward_probe — the guest tries to leave. Three echo requests, every one of which must die
# in the forward chain and be counted there by the kernel.
forward_probe() {
  nsenter -t "$guest_pid" -n ping -c 3 -W 1 10.99.99.2 >/dev/null 2>&1 || true
}

inner_main() {
  local scenario="$1"
  work="${SKIFF_HARNESS_WORK:?internal: SKIFF_HARNESS_WORK unset}"
  rundir="$work/$scenario/run.d"
  mkdir -p "$rundir"
  trap inner_cleanup EXIT

  local proof_count=1
  case "$scenario" in
  unprivileged)
    echo "harness: the unprivileged scenario runs outside the namespace" >&2
    return 1
    ;;
  no-table)
    # Nothing raised the guard at all: proof must not mistake a bare host for a guarded one.
    lo_up
    health_up
    console_log "$blocked_line"
    ;;
  baseline-pass)
    guard_up
    lo_up
    health_up
    console_log "$blocked_line"
    ;;
  forward-chain-deleted)
    guard_up
    nft flush chain inet skiff forward
    nft delete chain inet skiff forward
    lo_up
    health_up
    console_log "$blocked_line"
    ;;
  input-chain-deleted)
    guard_up
    nft flush chain inet skiff input
    nft delete chain inet skiff input
    lo_up
    health_up
    console_log "$blocked_line"
    ;;
  forwarding-on-zero-counters)
    guard_up
    lo_up
    health_up
    console_log "$blocked_line"
    sysctl -qw net.ipv4.ip_forward=1
    ;;
  forwarding-on-real-counters)
    guard_up
    forwarding_guest_up
    health_up "$guest_pid"
    console_log "$blocked_line"
    forward_probe
    ;;
  escaped-wins)
    guard_up
    lo_up
    health_up
    # All three markers in one log, ESCAPED last: priority must be by meaning, not by order.
    console_log "$broken_line" "$blocked_line" "$escaped_line"
    ;;
  probe-broken)
    guard_up
    lo_up
    health_up
    # "Could not probe" must never be graded as "was prevented", even beside a BLOCKED line.
    console_log "$blocked_line" "$broken_line"
    ;;
  leak-demo-marker)
    # Otherwise a perfect passing run — the marker alone must stop the transcript dead.
    guard_up
    lo_up
    health_up
    console_log "$blocked_line"
    : >"$rundir/leak-demo.marker"
    ;;
  no-console-log)
    guard_up
    lo_up
    health_up
    ;;
  no-probe-line)
    guard_up
    lo_up
    health_up
    console_log "[    0.412032] random: crng init done" "llama-server: listening on 0.0.0.0:8080"
    ;;
  ipv6-enabled)
    guard_up
    lo_up
    health_up
    console_log "$blocked_line"
    sysctl -qw "net.ipv6.conf.skiff-tap0.disable_ipv6=0"
    ;;
  health-down)
    guard_up
    lo_up
    console_log "$blocked_line"
    ;;
  tap-missing)
    guard_up
    ip link del skiff-tap0
    lo_up
    health_up
    console_log "$blocked_line"
    ;;
  count-guard)
    guard_up
    lo_up
    health_up
    console_log "$blocked_line"
    proof_count=99999999999999999999999999999999
    ;;
  *)
    echo "harness: unknown scenario '$scenario'" >&2
    return 1
    ;;
  esac

  local rc=0
  SKIFF_RUN_DIR="$rundir" "$proof" "$proof_count" || rc=$?
  # Sentinel: the outer half refuses to grade any scenario whose fixture died before this
  # line, so a broken fixture can never be counted as a scenario that "correctly failed".
  printf '[harness] scenario %s: proof exit %d\n' "$scenario" "$rc"
  return "$rc"
}

if [[ "${1:-}" == "--scenario" ]]; then
  inner_main "${2:?scenario name required}"
  exit $?
fi

# =============================================================================
# OUTER — preflight, scenario table, verdict.
# =============================================================================
work=""
outer_cleanup() {
  [[ -n "$work" ]] && rm -rf "$work"
  return 0
}

total=0
failed=0

report() {
  local name="$1" verdict="$2" detail="$3"
  printf '%-28s %-6s %s\n' "$name" "$verdict" "$detail"
  [[ "$verdict" == "OK" ]] || failed=$((failed + 1))
}

# assert_output <name> <file> <observed rc> <expected rc> <pattern>...
# A pattern prefixed with '!' must be ABSENT; every other pattern must be present.
# Patterns are matched as fixed strings, so an assertion cannot pass by regex accident.
assert_output() {
  local name="$1" out="$2" rc="$3" want="$4"
  shift 4
  if [[ "$rc" != "$want" ]]; then
    report "$name" "FAIL" "expected exit ${want}, got ${rc}"
    sed -n '1,200p' "$out" | sed 's/^/    | /'
    return 0
  fi
  local pat
  for pat in "$@"; do
    if [[ "${pat:0:1}" == "!" ]]; then
      if grep -qF -- "${pat:1}" "$out"; then
        report "$name" "FAIL" "output must NOT contain: ${pat:1}"
        return 0
      fi
    elif ! grep -qF -- "$pat" "$out"; then
      report "$name" "FAIL" "output missing: ${pat}"
      return 0
    fi
  done
  report "$name" "OK" "exit ${rc}, all ${#} assertion(s) matched"
}

# check <name> <expected rc> <pattern>... — run one scenario in its own namespace.
check() {
  local name="$1" want="$2"
  shift 2
  total=$((total + 1))
  local out="$work/${name}.out" rc=0
  # `bash "$self"` rather than exec'ing it: the bundle is designed to ship on vfat/exFAT,
  # where the executable bit is a mount property and may not survive the copy.
  SKIFF_HARNESS_WORK="$work" unshare -rn bash "$self" --scenario "$name" >"$out" 2>&1 || rc=$?
  if ! grep -qF -- "[harness] scenario ${name}: proof exit ${rc}" "$out"; then
    report "$name" "ERROR" "fixture failed before proof ran (no sentinel) — scenario not graded"
    sed -n '1,200p' "$out" | sed 's/^/    | /'
    return 0
  fi
  assert_output "$name" "$out" "$rc" "$want" "$@"
}

main() {
  local tool
  for tool in unshare nft ip nsenter curl python3 sed grep; do
    command -v "$tool" >/dev/null || {
      echo "harness: missing required tool: $tool" >&2
      exit 1
    }
  done
  [[ -x "$proof" ]] || {
    echo "harness: no executable proof at $proof" >&2
    exit 1
  }
  unshare -rn true 2>/dev/null || {
    echo "harness: 'unshare -rn' is not permitted here — this host has unprivileged user" >&2
    echo "         namespaces disabled, and the harness needs one to own a network stack." >&2
    exit 1
  }

  work="$(mktemp -d)"
  trap outer_cleanup EXIT

  echo "== skiff proof harness — $(date -u +%FT%TZ) =="
  echo "repo:      $repo"
  echo "isolation: unshare -rn — root in a private user+net namespace, one per scenario"
  echo "guard:     built by running netsetup itself, so the rules under test are its own"
  echo "host:      untouched — no host interface, sysctl, nftables rule or bundle file is written"
  echo ""

  # 1. Unprivileged refusal. Runs OUTSIDE any namespace, as the invoking user, because
  #    inside `unshare -r` there is no non-root uid to run as.
  total=$((total + 1))
  local out="$work/unprivileged.out" rc=0
  "$proof" 1 >"$out" 2>&1 || rc=$?
  assert_output "unprivileged" "$out" "$rc" 1 \
    "proof: nftables state is root-listable only" \
    "!PROOF PASSED"

  # 2. The guard is simply not there.
  check no-table 1 \
    "FAIL: forward chain missing its iifname counter-drop rule" \
    "FAIL: forward chain missing its oifname counter-drop rule" \
    "FAIL: input chain missing the established-only counter-accept rule" \
    "FAIL: input chain missing its counter-drop rule" \
    "== PROOF FAILED" \
    "!PROOF PASSED"

  # 3. The direction that must still be able to pass: everything correct, forwarding off.
  check baseline-pass 0 \
    "OK: no leak-demo marker" \
    "net.ipv4.ip_forward = 0" \
    "OK: 0 packet(s) on the forward drops" \
    "EXPECTED AND CORRECT reading, not a gap" \
    "OK: reachable from host (172.30.0.2:8080)" \
    "OK: skiff-tap0 has IPv6 disabled (disable_ipv6=1)" \
    "== PROOF PASSED" \
    "!FAIL"

  # 4. One chain deleted at a time — the per-chain assertions must not cover for each other.
  check forward-chain-deleted 1 \
    "FAIL: forward chain missing its iifname counter-drop rule" \
    "FAIL: forward chain missing its oifname counter-drop rule" \
    "FAIL: no forward-chain counter lines to read" \
    "!input chain missing"
  check input-chain-deleted 1 \
    "FAIL: input chain missing the established-only counter-accept rule" \
    "FAIL: input chain missing its counter-drop rule" \
    "!forward chain missing"

  # 5. Check 2b in both directions, against the live forwarding posture.
  check forwarding-on-zero-counters 1 \
    "FAIL: ip_forward is '1', want 0" \
    "FAIL: forward drop counters are ZERO while net.ipv4.ip_forward = '1'" \
    "!EXPECTED AND CORRECT"
  check forwarding-on-real-counters 1 \
    "FAIL: ip_forward is '1', want 0" \
    "packet(s) hit the forward drops — the guest tried, the kernel stopped it" \
    "!counters are ZERO"

  # 6. Marker priority: ESCAPED and PROBE-BROKEN each beat a BLOCKED line in the same log.
  check escaped-wins 1 \
    "FAIL: GUEST REACHED THE OUTSIDE WORLD" \
    "ISOLATION BROKEN" \
    "!corroboration only"
  check probe-broken 1 \
    "FAIL: the guest's evidence machinery died" \
    "!corroboration only"

  # 7. Leak-demo mode is never a real run — and it stops the transcript before check 1.
  check leak-demo-marker 1 \
    "leak-demo mode — this is never a real run" \
    "!-- 1. Host is not forwarding --" \
    "!PROOF PASSED"

  # 8. Missing or silent guest evidence is a failure, never a pass by omission.
  check no-console-log 1 "FAIL: no console log at"
  check no-probe-line 1 "FAIL: no probe line in"

  # 9. The per-instance host-side checks.
  check ipv6-enabled 1 "FAIL: skiff-tap0 disable_ipv6 = '0', want 1"
  check health-down 1 "FAIL: instance 0 not serving"
  check tap-missing 1 "FAIL: no skiff TAPs found"

  # 10. The count guard, with a value that wraps signed 64-bit arithmetic (task-5 F-1).
  check count-guard 1 \
    "proof: count must be an integer in 0..256" \
    "!PROOF PASSED"

  echo ""
  if [[ "$failed" -eq 0 ]]; then
    echo "== HARNESS PASSED: ${total} scenarios, every one behaved exactly as asserted =="
    return 0
  fi
  echo "== HARNESS FAILED: ${failed} of ${total} scenarios did not behave as asserted =="
  return 1
}

main "$@"
