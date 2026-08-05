# skiff ablation matrix — results

Run 2026-08-05T10:56:02Z on `Linux 7.1.3-200.fc44.x86_64`, 24 CPUs.

> **Regenerated for publication.** This summary is identical to the one the runner emitted except
> that the operator's absolute bundle path and Unix username have been omitted — they disclose a
> filesystem layout and carry no evidentiary value. **No measurement, verdict, exit code or counter
> value differs.** The transcripts, launcher logs and ruleset captures in this directory are the
> unmodified output of the run; `test/ablation.sh` now emits the basename only, so future runs need
> no such note.

| | |
|---|---|
| Bundle | `stick/` (assembled medium) |
| Bundle integrity gate | ENFORCED — MANIFEST.sha256 present (10 files) |
| `proof` provenance | byte-identical to the `proof` published in this repository (enforced at start) |
| Inference throughput (baseline row, shipped posture) | 28.2 tok/s (128 tokens, 5.56 s wall) |

The launcher ran unprivileged; every row applied its toggle **before** the boot, so the guest's own
probe ran under the condition being tested. `nft list ruleset` is captured before and after the
toggle for every row, so the single-variable claim is checkable.

| # | Control toggled | proof exit | forward-drop counters | Observed |
|---|---|---|---|---|
| 1 | baseline | 0 | 0 | PASSED (expected) |
| 2 | forward chain deleted | 1 | n/a (forward chain absent) | FAILED (expected) |
| 3 | input chain deleted | 1 | 0 | FAILED (expected) |
| 4 | ip_forward=1 only | 1 | 3 | FAILED (expected) · COUNTERS NON-ZERO — drop rules proven load-bearing on real hardware |
| 5 | leak-demo, no NAT | 1 (marker) / 0 (graded) | 0 | marker refusal fired (expected) |
| 6 | full leak | 1 | n/a (forward chain absent) | FAILED (expected) · ESCAPED — genuine outbound egress [host firewall exempted for the TAP] |

## Evidence files

Per row: `rowN-ruleset-before.txt`, `rowN-ruleset-applied.txt`, `rowN-proof.txt`, `rowN-boot.log`.
Rows 5 and 6 additionally carry `rowN-proof-marker.txt` — the leak-demo marker refusal, captured
before the marker was removed to obtain a gradeable transcript. That removal is deliberate and is
recorded here rather than left implicit.

**One artifact is withheld, and this is why.** The raw completion sample behind the 28.2 tok/s
figure (`row1-inference.json`) is not published: the prompt used in that run contained wording
reserved to a separate, confidential submission, and a language model echoes its prompt back into
its output. The number quoted above is `timings.predicted_per_second` as reported by the inference
server in that response. `test/ablation.sh` now uses a generic prompt, so a re-run publishes the
sample in full. Withholding it is a disclosure boundary, not a measurement caveat — no timing
figure changes.

## Notes carried from the runner

- Row 5 cannot deliver "counters above baseline" as originally specified: `proof` stops at check 0
  on the marker, and with `ip_forward=0` the forward hook is never traversed, so the counters are
  structurally zero. Counters above are read directly from nft for every row and report what was
  actually observed.
- Row 6 keeps `netsetup.state` while deleting the nftables guard. The runbook's Part 2 calls
  `netsetup down` and then `run up`, which cannot boot: the launcher refuses without that file.
- **Row 6 ran with `SKIFF_FIREWALLD_ALLOW=1`**: the skiff TAP was parked in firewalld's `trusted`
  zone (runtime only) for that row. Without it, measured 2026-08-05, the guest is blocked by
  firewalld's `filter_FORWARD`, which ends in `reject with icmpx admin-prohibited` — **not** by
  anything skiff does. Row 6 asks whether the guest escapes when *skiff's* guard is gone, so an
  unrelated host firewall rejecting the traffic measures the wrong thing. This is a second variable
  and is named here rather than folded into the row silently.
