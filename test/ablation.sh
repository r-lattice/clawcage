#!/usr/bin/env bash
# ablation.sh — run the six-row single-variable ablation matrix on real hardware.
#
# This is the experiment the README publishes as a DESIGN and marks "not yet run".
# One control is toggled per row, applied BEFORE the boot so the guest's own probe
# runs under the condition being tested, with `nft list ruleset` captured both before
# and after the toggle so the single-variable claim is checkable rather than asserted.
#
# Usage:  sudo ./test/ablation.sh
#
# Why a script and not the hand-typed runbook: the runbook is ~40 steps across six
# rows with a full boot cycle each, and a mistyped step silently produces a row that
# looks like a result. This drives the same commands, captures the evidence for every
# row, and refuses to continue when a precondition is not met.
#
# ---------------------------------------------------------------------------------
# Two facts about this experiment that the README's design table does not say:
#
# 1. THE RUNBOOK'S PART 2 CANNOT WORK AS WRITTEN. It calls `netsetup down` (which
#    deletes netsetup.state) and then `./run up`, whose checkNetsetupState gate
#    refuses to boot without that file. Row 6 here removes the GUARD (the nftables
#    table) while leaving the STATE FILE, which is a genuinely unguarded host and is
#    the only way the launcher will boot into one.
#
# 2. ROW 5 CANNOT DELIVER "COUNTERS ABOVE BASELINE" AS SPECIFIED. `proof` stops at
#    check 0 on the leak-demo marker and never reaches the counter check; and with
#    ip_forward=0 the kernel rejects a non-local guest packet at the routing decision,
#    so the forward hook is never traversed and the counters are structurally zero
#    anyway. This script reads the counters directly from nft for every row and
#    records what they actually were, rather than reporting the expectation.
#
# For rows 5 and 6 the marker refusal is captured FIRST, then the marker is removed
# and proof re-run to obtain a gradeable transcript. That removal is deliberate,
# logged in the output, and is what produced the two published Aug-1 transcripts.
# ---------------------------------------------------------------------------------

set -euo pipefail

# --- preconditions -----------------------------------------------------------

if [[ $EUID -ne 0 ]]; then
  echo "ablation: needs root (netsetup, nft and proof are root-only)" >&2
  echo "run: sudo ./test/ablation.sh" >&2
  exit 1
fi

RUNUSER="${SUDO_USER:-}"
if [[ -z "$RUNUSER" || "$RUNUSER" == "root" ]]; then
  echo "ablation: SUDO_USER is unset or root — the launcher must run unprivileged." >&2
  echo "run this via sudo from your normal user account." >&2
  exit 1
fi

REPO="$(cd "$(dirname "$0")/.." && pwd)"

# SKIFF_BUNDLE_DIR drives the experiment from somewhere other than the dev tree --
# specifically from the assembled `stick/`, which carries MANIFEST.sha256. That matters:
# run 1 of this matrix executed from the dev tree, where the launcher prints
# "no MANIFEST.sha256 — unverified bundle" and boots anyway, so the SHA-256 integrity
# gate the write-up presents as a headline control was never exercised by the experiment
# that the write-up cites. Evidence still lands in the REPO, never inside the bundle,
# so it cannot perturb the manifest it is meant to be evidence about.
BUNDLE="${SKIFF_BUNDLE_DIR:-$REPO}"
[[ -d "$BUNDLE" ]] || { echo "ablation: SKIFF_BUNDLE_DIR '$BUNDLE' is not a directory" >&2; exit 1; }
BUNDLE="$(cd "$BUNDLE" && pwd)"
cd "$BUNDLE"

for f in ./netsetup ./proof ./run ./config.yaml; do
  [[ -e "$f" ]] || { echo "ablation: missing $f in $BUNDLE" >&2; exit 1; }
done

if [[ -e MANIFEST.sha256 ]]; then
  INTEGRITY="ENFORCED — MANIFEST.sha256 present ($(wc -l < MANIFEST.sha256) files)"
else
  INTEGRITY="NOT ENFORCED — no MANIFEST.sha256 in this bundle (dev tree); the launcher will warn and boot anyway"
fi

# The published tool and the development copy must be the same bytes, or the transcripts
# are not reproducible by a reader running what was published. Run 1 failed this: its
# transcripts carried an internal codename the public `proof` does not contain.
if [[ -e "$REPO/.publish/proof" ]] && ! cmp -s ./proof "$REPO/.publish/proof"; then
  echo "ablation: ./proof differs from the PUBLISHED .publish/proof." >&2
  echo "Transcripts from this run would not be reproducible against the public repository." >&2
  diff ./proof "$REPO/.publish/proof" >&2 || true
  exit 1
fi

COUNT="$(awk '/^instances:/ {print $2; exit}' config.yaml)"
if ! [[ "$COUNT" =~ ^[1-9][0-9]?$ ]]; then
  echo "ablation: could not read a sane instance count from config.yaml (got '${COUNT}')" >&2
  exit 1
fi
if [[ "$COUNT" != "1" ]]; then
  echo "ablation: config.yaml asks for ${COUNT} instances; this matrix is written for 1." >&2
  echo "set 'instances: 1' in config.yaml and re-run." >&2
  exit 1
fi

# `run` is a build artifact rebuilt from cmd/ + internal/. Booting a binary older than
# its sources means measuring code that is not the code in the tree -- the README records
# this costing a real debugging cycle (a kernel-panic fix already correct in vm.go, with
# the next boot panicking identically because the binary predated it by seven minutes).
# An ablation run is exactly where that must not happen silently: the transcripts become
# published evidence about a build nobody can reconstruct.
mapfile -t STALE_SRC < <(find "$REPO/cmd" "$REPO/internal" -name '*.go' ! -name '*_test.go' -newer "$BUNDLE/run" 2>/dev/null)
if ((${#STALE_SRC[@]} > 0)); then
  echo "ablation: the 'run' binary is OLDER than these sources:" >&2
  printf '  %s\n' "${STALE_SRC[@]}" >&2
  echo >&2
  echo "Rebuild first:  GOTOOLCHAIN=local go build -o run ./cmd/run" >&2
  echo >&2
  echo "If you have verified the differences are non-functional (comments only), re-run as:" >&2
  echo "  sudo SKIFF_ALLOW_STALE_RUN=1 ./test/ablation.sh" >&2
  echo "The override is recorded in RESULTS.md so the evidence says which build produced it." >&2
  [[ "${SKIFF_ALLOW_STALE_RUN:-}" == "1" ]] || exit 1
  echo "ablation: SKIFF_ALLOW_STALE_RUN=1 — proceeding on an out-of-date binary." >&2
  echo >&2
fi

RUNDIR="${SKIFF_RUN_DIR:-/tmp/skiff-run.d}"
STAMP="$(date -u +%Y-%m-%d)"
OUT="$REPO/docs/ablation-$STAMP"
mkdir -p "$OUT"

BOOT_PID=""
ORIG_FORWARD="$(sysctl -n net.ipv4.ip_forward)"
FIREWALLD_TAP=""          # set when a TAP has been parked in firewalld's trusted zone

# Optional row selection: `sudo ./test/ablation.sh 6` re-runs one row.
ROWS=("$@")
if ((${#ROWS[@]} == 0)); then
  ROWS=(1 2 3 4 5 6)
  PARTIAL=0
else
  PARTIAL=1
  for r in "${ROWS[@]}"; do
    [[ "$r" =~ ^[1-6]$ ]] || { echo "ablation: row must be 1..6, got '$r'" >&2; exit 1; }
  done
fi

# Row 6 needs a host with no protections OTHER than skiff's, or it measures the wrong
# thing. Measured 2026-08-05: with skiff's guard deleted, forwarding on and NAT in place,
# the guest was still blocked -- by firewalld's filter_FORWARD, which ends in
# `reject with icmpx admin-prohibited`. No VPN table was present at all, so the README's
# "the VPN blocks it" is too narrow: any host firewall manager does this.
#
# The allowance parks ONLY the skiff TAP in firewalld's trusted zone, at runtime (not
# --permanent), and only for row 6. Every other interface keeps its policy, and the
# cleanup trap removes it however the script exits.
firewalld_allow() {
  local tap="skiff-tap0"
  command -v firewall-cmd >/dev/null 2>&1 || {
    echo "   firewalld allowance requested but firewall-cmd is absent — skipping" >&2
    return 0
  }
  systemctl is-active --quiet firewalld || {
    echo "   firewalld is not active — no allowance needed" >&2
    return 0
  }
  if firewall-cmd --quiet --zone=trusted --change-interface="$tap" 2>/dev/null; then
    FIREWALLD_TAP="$tap"
    echo "   firewalld: $tap parked in the 'trusted' zone (runtime only, removed on exit)"
  else
    echo "   firewalld: could not move $tap to the trusted zone — row 6 may not escape" >&2
  fi
}

firewalld_restore() {
  [[ -n "$FIREWALLD_TAP" ]] || return 0
  firewall-cmd --quiet --zone=trusted --remove-interface="$FIREWALLD_TAP" 2>/dev/null || true
  echo "   firewalld: $FIREWALLD_TAP removed from the 'trusted' zone."
  FIREWALLD_TAP=""
}

echo "ablation matrix — $(date -u +%FT%TZ)"
echo "  bundle    $BUNDLE"
echo "  launcher  runs as $RUNUSER"
echo "  output    $OUT"
echo "  integrity $INTEGRITY"
echo "  host ip_forward on entry: $ORIG_FORWARD (restored on exit)"
echo

# --- helpers -----------------------------------------------------------------

stop_vm() {
  [[ -n "$BOOT_PID" ]] || return 0
  if kill -0 "$BOOT_PID" 2>/dev/null; then
    kill -INT "$BOOT_PID" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "$BOOT_PID" 2>/dev/null || break
      sleep 0.2
    done
    kill -0 "$BOOT_PID" 2>/dev/null && kill -KILL "$BOOT_PID" 2>/dev/null || true
  fi
  wait "$BOOT_PID" 2>/dev/null || true
  BOOT_PID=""
}

reset_host() {
  stop_vm
  ./netsetup down "$COUNT" >/dev/null 2>&1 || true
  nft delete table ip skiffleak 2>/dev/null || true
  nft delete table inet skiff 2>/dev/null || true
  sysctl -qw net.ipv4.ip_forward=0
  rm -rf "$RUNDIR"
  rm -f "$BUNDLE/netsetup.state"
}

# Restore the host no matter how this script leaves.
cleanup() {
  local rc=$?
  trap - EXIT INT TERM     # cleanup calls exit; without this it re-enters itself
  echo
  echo "-- restoring host --"
  stop_vm
  firewalld_restore
  ./netsetup down "$COUNT" >/dev/null 2>&1 || true
  nft delete table ip skiffleak 2>/dev/null || true
  nft delete table inet skiff 2>/dev/null || true
  sysctl -qw "net.ipv4.ip_forward=$ORIG_FORWARD"
  rm -rf "$RUNDIR"
  echo "   ip_forward restored to $ORIG_FORWARD; skiff and skiffleak tables removed."
  exit $rc
}
trap cleanup EXIT INT TERM

capture_ruleset() {
  # Full host ruleset, not just skiff's table: the single-variable claim is about the
  # whole firewall state, and another daemon's table (firewalld, a VPN) changing
  # underneath a row would otherwise be invisible.
  { echo "== nft list ruleset — $(date -u +%FT%TZ) =="; nft list ruleset 2>&1; } > "$1"
}

# Forward-drop packet total, read straight from the kernel. Prints "n/a" when the
# chain is absent, which is itself a result for rows 2 and 6.
fwd_counter_total() {
  local chain
  chain="$(nft list chain inet skiff forward 2>/dev/null || true)"
  if [[ -z "$chain" ]]; then
    echo "n/a (forward chain absent)"
    return
  fi
  grep -E '(iifname|oifname) "skiff-tap\*" counter' <<<"$chain" \
    | awk '{for (i = 1; i <= NF; i++) if ($i == "packets") s += $(i + 1)} END {print s + 0}'
}

boot_vm() {
  local leak="$1" log="$2"
  : > "$log"
  # The launcher must be unprivileged: the TAP is created with `user $RUNUSER`, and
  # firecracker has to be able to open it. It also stays in the foreground by design,
  # so it is backgrounded here and stopped after proof has read the console logs --
  # `run up` wipes the run dir on exit, console logs included.
  # SC2024: the redirect is performed by THIS shell, which is root — that is intended,
  # so every evidence file in the output directory is root-owned and the unprivileged
  # launcher cannot rewrite its own transcript.
  # shellcheck disable=SC2024
  sudo -u "$RUNUSER" env SKIFF_LEAK_DEMO="$leak" ./run up >"$log" 2>&1 &
  BOOT_PID=$!
  for _ in $(seq 1 300); do          # up to 150 s; WaitReady itself allows 120 s
    if grep -q 'READY' "$log" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$BOOT_PID" 2>/dev/null; then
      echo "   BOOT FAILED — launcher exited before READY. Tail:" >&2
      tail -n 25 "$log" >&2
      BOOT_PID=""
      return 1
    fi
    sleep 0.5
  done
  echo "   BOOT TIMED OUT waiting for READY. Tail:" >&2
  tail -n 25 "$log" >&2
  return 1
}

# Inference throughput. An appliance whose pitch is "it runs a model locally" has to say
# what it actually serves; run 1 captured boot time and nothing else, which is the first
# number an evaluator asks for. Measured on the BASELINE row only -- guard intact, the
# shipped posture -- so the figure describes the configuration that would be deployed.
#
# Keep the prompt GENERIC. The response file is published, and a model echoes its prompt
# back: the 2026-08-05 run used wording reserved to a separate confidential submission,
# and the publish audit correctly refused the push over it.
THROUGHPUT="not measured"
measure_inference() {
  local out="$1" t0 t1 wall toks tps
  t0="$(date +%s.%N)"
  if ! curl -fsS -m 180 "http://172.30.0.2:8080/v1/completions" \
        -H 'Content-Type: application/json' \
        -d '{"prompt":"Explain in one paragraph how a hypervisor boundary differs from a container boundary.","max_tokens":128,"temperature":0}' \
        > "$out" 2>/dev/null; then
    THROUGHPUT="request failed"
    return 0
  fi
  t1="$(date +%s.%N)"
  wall="$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b - a}')"
  # llama-server reports its own timings; prefer them over wall clock, which includes
  # request overhead. Fall back to tokens/wall, then to "served but uncounted".
  tps="$(jq -r '.timings.predicted_per_second // empty' "$out" 2>/dev/null || true)"
  toks="$(jq -r '.usage.completion_tokens // .timings.predicted_n // empty' "$out" 2>/dev/null || true)"
  if [[ "$tps" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    THROUGHPUT="$(awk -v t="$tps" 'BEGIN{printf "%.1f", t}') tok/s (${toks:-?} tokens, ${wall}s wall)"
  elif [[ "$toks" =~ ^[0-9]+$ ]] && [[ "$wall" != "0.00" ]]; then
    THROUGHPUT="$(awk -v t="$toks" -v w="$wall" 'BEGIN{printf "%.1f", t/w}') tok/s (${toks} tokens, ${wall}s wall)"
  else
    THROUGHPUT="served, no token counts in response (${wall}s wall)"
  fi
}

take_proof() {
  local out="$1" rc=0
  set +e
  ./proof "$COUNT" > "$out" 2>&1
  rc=$?
  set -e
  return $rc
}

# Row bookkeeping.
declare -a R_NUM R_NAME R_EXIT R_COUNTERS R_VERDICT

record() {
  R_NUM+=("$1"); R_NAME+=("$2"); R_EXIT+=("$3"); R_COUNTERS+=("$4"); R_VERDICT+=("$5")
  printf '   proof exit %s · forward-drop counters: %s · %s\n' "$3" "$4" "$5"
}

banner() {
  echo
  echo "=============================================================="
  echo " ROW $1 — $2"
  echo "=============================================================="
}

# --- rows --------------------------------------------------------------------
# Every row: reset -> guard up -> capture before -> apply ONE toggle -> capture
# after -> boot -> read counters -> proof -> record.

row_common_setup() {
  local n="$1"
  reset_host
  ./netsetup up "$COUNT" "$RUNUSER" >/dev/null
  capture_ruleset "$OUT/row${n}-ruleset-before.txt"
}

# Row 1 — baseline, nothing toggled.
row1() {
  banner 1 "baseline (no control removed)"
  row_common_setup 1
  cp "$OUT/row1-ruleset-before.txt" "$OUT/row1-ruleset-applied.txt"
  boot_vm 0 "$OUT/row1-boot.log" || { record 1 "baseline" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  local rc=0; take_proof "$OUT/row1-proof.txt" || rc=$?
  measure_inference "$OUT/row1-inference.json"
  echo "   inference: $THROUGHPUT"
  record 1 "baseline" "$rc" "$c" "$([[ $rc -eq 0 ]] && echo 'PASSED (expected)' || echo 'FAILED — expected pass')"
}

# Row 2 — forward chain deleted.
row2() {
  banner 2 "forward chain deleted"
  row_common_setup 2
  nft delete chain inet skiff forward
  capture_ruleset "$OUT/row2-ruleset-applied.txt"
  boot_vm 0 "$OUT/row2-boot.log" || { record 2 "forward chain deleted" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  local rc=0; take_proof "$OUT/row2-proof.txt" || rc=$?
  record 2 "forward chain deleted" "$rc" "$c" "$([[ $rc -ne 0 ]] && echo 'FAILED (expected)' || echo 'PASSED — expected failure')"
}

# Row 3 — input chain deleted.
row3() {
  banner 3 "input chain deleted"
  row_common_setup 3
  nft delete chain inet skiff input
  capture_ruleset "$OUT/row3-ruleset-applied.txt"
  boot_vm 0 "$OUT/row3-boot.log" || { record 3 "input chain deleted" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  local rc=0; take_proof "$OUT/row3-proof.txt" || rc=$?
  record 3 "input chain deleted" "$rc" "$c" "$([[ $rc -ne 0 ]] && echo 'FAILED (expected)' || echo 'PASSED — expected failure')"
}

# Row 4 — ip_forward=1 only. THE LOAD-BEARING ROW: the guard is fully present, so a
# probing guest must now traverse the forward hook and leave a mark on the counters.
# This is the only row that makes the drop rules load-bearing on real hardware
# (elsewhere it is shown only in the namespaced harness).
row4() {
  banner 4 "ip_forward=1 only (guard intact)"
  row_common_setup 4
  sysctl -qw net.ipv4.ip_forward=1
  capture_ruleset "$OUT/row4-ruleset-applied.txt"
  echo "net.ipv4.ip_forward = 1 (toggled)" >> "$OUT/row4-ruleset-applied.txt"
  boot_vm 0 "$OUT/row4-boot.log" || { record 4 "ip_forward=1" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  local rc=0; take_proof "$OUT/row4-proof.txt" || rc=$?
  local note="FAILED (expected)"
  [[ $rc -eq 0 ]] && note="PASSED — expected failure"
  if [[ "$c" =~ ^[0-9]+$ ]] && ((c > 0)); then
    note="$note · COUNTERS NON-ZERO — drop rules proven load-bearing on real hardware"
  fi
  record 4 "ip_forward=1 only" "$rc" "$c" "$note"
}

# Row 5 — leak-demo boot args, guard up, no host NAT.
row5() {
  banner 5 "leak-demo boot args, guard up, no NAT"
  row_common_setup 5
  capture_ruleset "$OUT/row5-ruleset-applied.txt"
  echo "toggle: SKIFF_LEAK_DEMO=1 at boot (no firewall change)" >> "$OUT/row5-ruleset-applied.txt"
  boot_vm 1 "$OUT/row5-boot.log" || { record 5 "leak-demo, no NAT" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  # First: the marker refusal, which is the designed behaviour and its own evidence.
  local rc_m=0; take_proof "$OUT/row5-proof-marker.txt" || rc_m=$?
  # Then remove the marker to obtain a gradeable transcript. Deliberate and logged.
  echo "   (removing leak-demo.marker to obtain a gradeable transcript — logged)"
  rm -f "$RUNDIR/leak-demo.marker"
  local rc=0; take_proof "$OUT/row5-proof.txt" || rc=$?
  record 5 "leak-demo, no NAT" "$rc_m (marker) / $rc (graded)" "$c" \
    "$([[ $rc_m -ne 0 ]] && echo 'marker refusal fired (expected)' || echo 'marker refusal DID NOT fire')"
}

# Row 6 — full leak: guard removed, forwarding on, NAT in place, leak-demo boot.
# netsetup.state is left in place deliberately -- see the header note. The host is
# genuinely unguarded here; this is the row the VPN used to block.
row6() {
  banner 6 "full leak (guard down + forwarding + NAT)"
  reset_host
  ./netsetup up "$COUNT" "$RUNUSER" >/dev/null      # creates TAP + table + state
  capture_ruleset "$OUT/row6-ruleset-before.txt"
  nft delete table inet skiff                        # remove the GUARD, keep the state file
  sysctl -qw net.ipv4.ip_forward=1
  nft add table ip skiffleak
  nft add chain ip skiffleak post '{ type nat hook postrouting priority 100 ; }'
  nft add rule ip skiffleak post ip saddr 172.30.0.0/16 masquerade
  [[ "${SKIFF_FIREWALLD_ALLOW:-}" == "1" ]] && firewalld_allow
  capture_ruleset "$OUT/row6-ruleset-applied.txt"
  echo "net.ipv4.ip_forward = 1 (toggled); inet skiff removed; ip skiffleak NAT added" \
    >> "$OUT/row6-ruleset-applied.txt"
  boot_vm 1 "$OUT/row6-boot.log" || { record 6 "full leak" "BOOT-FAIL" "n/a" "boot failed"; return 0; }
  local c; c="$(fwd_counter_total)"
  local rc_m=0; take_proof "$OUT/row6-proof-marker.txt" || rc_m=$?
  echo "   (removing leak-demo.marker to obtain a gradeable transcript — logged)"
  rm -f "$RUNDIR/leak-demo.marker"
  local rc=0; take_proof "$OUT/row6-proof.txt" || rc=$?
  # The row states its own host-firewall condition. Run 1 produced two result files whose
  # row 6 lines flatly contradicted each other -- both were honest records of different
  # host conditions, but neither said which, so together they read as a contradiction.
  # A row that carries its own preconditions cannot do that.
  local fw="host firewall NOT exempted"
  [[ -n "$FIREWALLD_TAP" ]] && fw="host firewall exempted for the TAP"
  local note="FAILED (expected)"
  [[ $rc -eq 0 ]] && note="PASSED — expected failure"
  if grep -q 'SKIFF-OUTBOUND-ESCAPED' "$OUT/row6-proof.txt" 2>/dev/null; then
    note="$note · ESCAPED — genuine outbound egress [$fw]"
  else
    note="$note · no ESCAPED — the guest was stopped by the HOST firewall, not by skiff [$fw]"
  fi
  record 6 "full leak" "$rc" "$c" "$note"
}

# --- drive -------------------------------------------------------------------

for r in "${ROWS[@]}"; do
  "row${r}"
done

# --- results -----------------------------------------------------------------

# A partial run must never overwrite the full matrix's results file.
if ((PARTIAL)); then
  RESULTS="$OUT/RESULTS-row$(IFS=_; echo "${ROWS[*]}").md"
else
  RESULTS="$OUT/RESULTS.md"
fi
{
  echo "# skiff ablation matrix — results"
  echo
  echo "Run $(date -u +%FT%TZ) on \`$(uname -sr)\`, $(nproc) CPUs."
  echo
  echo "| | |"
  echo "|---|---|"
  # Basename only, never the absolute path: this file is published, and an absolute
  # path discloses the operator's filesystem layout for no evidentiary gain.
  echo "| Bundle | \`$(basename "$BUNDLE")/\` (assembled medium) |"
  echo "| Bundle integrity gate | $INTEGRITY |"
  echo "| \`proof\` provenance | byte-identical to the published \`.publish/proof\` (enforced at start) |"
  echo "| Inference throughput (baseline row, shipped posture) | $THROUGHPUT |"
  echo
  echo "The launcher ran unprivileged; every row applied its toggle **before** the boot, so the"
  echo "guest's own probe ran under the condition being tested. \`nft list ruleset\` is captured"
  echo "before and after the toggle for every row, so the single-variable claim is checkable."
  echo
  echo "| # | Control toggled | proof exit | forward-drop counters | Observed |"
  echo "|---|---|---|---|---|"
  for i in "${!R_NUM[@]}"; do
    printf '| %s | %s | %s | %s | %s |\n' \
      "${R_NUM[$i]}" "${R_NAME[$i]}" "${R_EXIT[$i]}" "${R_COUNTERS[$i]}" "${R_VERDICT[$i]}"
  done
  echo
  echo "## Evidence files"
  echo
  echo "Per row: \`rowN-ruleset-before.txt\`, \`rowN-ruleset-applied.txt\`, \`rowN-proof.txt\`,"
  echo "\`rowN-boot.log\`. Rows 5 and 6 additionally carry \`rowN-proof-marker.txt\` — the"
  echo "leak-demo marker refusal, captured before the marker was removed to obtain a gradeable"
  echo "transcript. That removal is deliberate and is recorded here rather than left implicit."
  echo
  echo "## Notes carried from the runner"
  echo
  echo "- Row 5 cannot deliver \"counters above baseline\" as originally specified: \`proof\`"
  echo "  stops at check 0 on the marker, and with \`ip_forward=0\` the forward hook is never"
  echo "  traversed, so the counters are structurally zero. Counters above are read directly"
  echo "  from nft for every row and report what was actually observed."
  echo "- Row 6 keeps \`netsetup.state\` while deleting the nftables guard. The published"
  echo "  runbook's Part 2 calls \`netsetup down\` and then \`run up\`, which cannot boot:"
  echo "  the launcher refuses without that file."
  if [[ "${SKIFF_FIREWALLD_ALLOW:-}" == "1" ]]; then
    echo "- **Row 6 ran with \`SKIFF_FIREWALLD_ALLOW=1\`**: the skiff TAP was parked in"
    echo "  firewalld's \`trusted\` zone (runtime only) for that row. Without it, measured"
    echo "  2026-08-05, the guest is blocked by firewalld's \`filter_FORWARD\`, which ends in"
    echo "  \`reject with icmpx admin-prohibited\` — **not** by anything skiff does. Row 6 asks"
    echo "  whether the guest escapes when SKIFF's guard is gone, so an unrelated host firewall"
    echo "  rejecting the traffic measures the wrong thing. This is a second variable and is"
    echo "  named here rather than folded into the row silently."
  fi
  if [[ "${SKIFF_ALLOW_STALE_RUN:-}" == "1" ]]; then
    echo "- **The \`run\` binary was older than its Go sources and the staleness gate was"
    echo "  overridden with \`SKIFF_ALLOW_STALE_RUN=1\`.** Recorded here so the evidence states"
    echo "  which build produced it. Sources newer than the binary at run time:"
    # Backticks are literal markdown here, so single quotes are correct.
    # shellcheck disable=SC2016
    printf '  - `%s`\n' "${STALE_SRC[@]}"
  fi
} > "$RESULTS"

echo
echo "=============================================================="
echo " matrix complete"
echo "=============================================================="
column -t -s'|' < <(grep '^|' "$RESULTS") || cat "$RESULTS"
echo
echo "results  $RESULTS"
echo "evidence $OUT"
