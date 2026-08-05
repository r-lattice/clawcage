# clawcage — `skiff` v0.1

> The repository is **clawcage**, a nod to [OpenClawMachines](https://github.com/mathaix/OpenClawMachines),
> whose architecture patterns this project learned from. The tool itself is `skiff`, and that is the
> name every runtime identifier and published transcript uses.

A USB stick that boots a local LLM inside a Firecracker microVM with no configured path off the
host, plus a `proof` command that reads the host's own kernel state and prints what it finds. The
claim is **host-verifiable isolation evidence**: on a host in the documented configuration no egress
path exists from the guest, `proof` asserts each control per-chain and reports the kernel's own
enforcement counters, and a planned single-variable ablation matrix will show the assertion failing
as each control is removed.

Status: **v0.1, lab output.** The happy path has been run end-to-end on real hardware and its
transcript is published below verbatim. What has *not* been demonstrated end-to-end is named in
[Limitations](#limitations-v01) rather than smoothed over.

---

## Quickstart

Five commands. Each is single-purpose on purpose — no `&&` chains, so nothing hides inside a
one-liner you are about to run as root.

**1. Raise the guard** (the only step that needs root at setup time): one host-only TAP per
instance, plus the counted nftables deny table.

```
sudo ./netsetup up 1 $USER
```

**2. Drop the sudo timestamp.**

```
sudo -k
```

`sudo -k` is part of the workflow, not decoration. sudo caches a tty-scoped credential for five
minutes; a VMM escape landing in your shell during that window inherits it. Dropping the timestamp
before the untrusted thing starts is free.

**3. Boot** — unprivileged. Runs the pre-flight gates (memory headroom, `netsetup.state`, bundle
manifest), then boots the microVM and waits for the model server. Stays in the foreground; use a
second terminal for step 4.

```
./run up
```

Expect `instance 0: READY — http://172.30.0.2:8080`.

**4. Take the proof.** Root-only, because the kernel lists nftables state to root alone. `-E`
preserves `SKIFF_RUN_DIR` so `proof` reads the same run directory the launcher wrote.

```
sudo -E ./proof 1
```

Expect `== PROOF PASSED ==` and exit 0.

**5. Drop the timestamp again.**

```
sudo -k
```

Tear down with `sudo ./netsetup down 1` when finished; that removes the TAPs, the nftables table and
`netsetup.state`, and leaves no trace on the host.

`config.yaml` is the one knob file and it is safe to edit — see
[Bundle integrity](#bundle-integrity-manifestsha256) for why editing it does not trip the
integrity gate.

---

## Published evidence

### 1. The proof run — verbatim

`docs/proof-run-2026-08-01.txt`, exit 0, from the live run on the test rig described in
[Honest numbers](#honest-numbers). Nothing is elided:

```
== skiff isolation proof — 2026-08-01T12:04:02Z ==
run dir: /tmp/skiff-run.d

-- 0. leak-demo mode is NEVER a real run (S-6) --
OK: no leak-demo marker in /tmp/skiff-run.d

-- 1. Host is not forwarding --
net.ipv4.ip_forward = 0

-- 2. nftables: the two chains, checked separately (P-3) --
forward chain, verbatim:
table inet skiff {
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "skiff-tap*" counter packets 0 bytes 0 drop
		oifname "skiff-tap*" counter packets 0 bytes 0 drop
	}
}
input chain, verbatim:
table inet skiff {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "skiff-tap*" ct state established counter packets 126 bytes 12906 accept
		iifname "skiff-tap*" counter packets 0 bytes 0 drop
	}
}

-- 2b. forward drop counters, graded against the LIVE forwarding posture (P-2) --
counter evidence, verbatim:
		iifname "skiff-tap*" counter packets 0 bytes 0 drop
		oifname "skiff-tap*" counter packets 0 bytes 0 drop
OK: 0 packet(s) on the forward drops — with net.ipv4.ip_forward = 0 that is the
    EXPECTED AND CORRECT reading, not a gap: the kernel rejects a non-local guest packet
    at the routing decision and NEVER traverses the forward hook, so these counters cannot
    increment in the configuration skiff ships. ip_forward = 0 (check 1) is the primary
    control; these rules are defense in depth for the day forwarding is enabled, and their
    presence is check 2. The load-bearing test for the rules themselves is the ABLATION run
    with ip_forward=1, where this check demands non-zero and fails on zero.

-- 3. TAPs are host-only /30s --
skiff-tap0       UP             172.30.0.1/30 

-- 4.0 instance 0 --
OK: reachable from host (172.30.0.2:8080)
OK: skiff-tap0 has IPv6 disabled (disable_ipv6=1)
OK (corroboration only — guest-authored, not evidence against a malicious guest):
SKIFF-OUTBOUND-BLOCKED: default route via 172.30.0.1 installed, TCP 443 rc=1, UDP 53 reply bytes=0 — neither probe connected

== PROOF PASSED: 1 instance(s), host-only, forwarding off and forward-dropped ==
   Authoritative evidence is host-side kernel state: ip_forward (check 1) is the primary
   control, the forward drop rules (check 2) are defense in depth, and check 2b grades
   their counters against the live forwarding posture — zero is correct when forwarding
   is off, and required to be non-zero when it is on. The guest's own BLOCKED line
   corroborates all of it and never carries the verdict alone.
```

The same run served a completion: the model loaded from `/models/model.gguf` and answered over
`http://172.30.0.2:8080/v1/completions`, and llama-server reported build fingerprint `b1-876a432` —
the first eight characters of `LLAMA_CPP_COMMIT` in `build/pins.env`, i.e. the server in the image is
the commit the bundle pins. That console log is **not** reproduced here because it no longer exists:
the run directory (sockets, console logs, markers) is wiped by the launcher's own teardown, by
design. Only files that survived the run are published, and only verbatim.

### 2. Counter evidence — the one thing the guest cannot author

```
		iifname "skiff-tap*" counter packets 0 bytes 0 drop
		oifname "skiff-tap*" counter packets 0 bytes 0 drop
```

Every string beginning `SKIFF-OUTBOUND-` is written by the guest, so a hostile guest can write any
of them. These two lines come from the host kernel's own nftables counters, which no code inside the
VM can touch — a non-zero packet count there is the only evidence in this project that proves the
rule is load-bearing rather than merely present.

On this run it is **zero, and zero is the correct reading**, for the reason `proof` prints above:
with `ip_forward = 0` the kernel drops a non-local guest packet at the routing decision and never
reaches the forward hook, so the counter is structurally unreachable in the configuration skiff
ships. This was found by running it — check 2b originally demanded non-zero unconditionally and was
therefore unsatisfiable in the secure state; it now grades conditionally against the live
`ip_forward` value, and it is the ablation row with `ip_forward=1` that makes the counters
load-bearing. Non-zero counters *have* been observed, in `test/proof_harness.sh` — which raises
the real guard by running `netsetup` itself inside a private user+network namespace
(`unshare -rn`, no sudo, host untouched), forwards real packets from a namespaced peer, and runs
the real `proof` against the kernel's own counters. Its `forwarding-on-real-counters` scenario
prints:

```
OK: 3 packet(s) hit the forward drops — the guest tried, the kernel stopped it
```

That is harness evidence, not a live-host measurement, and is labeled as such. Reproduce it with
`bash test/proof_harness.sh`.

### 3. Ablation matrix — RUN 2026-08-05

Every row below is a result. Driven from the assembled `stick/`, so the `MANIFEST.sha256` integrity
gate actually executed — all six launcher logs record `bundle verified: 10 file(s)`. The `proof`
used is byte-identical to the one in this repository; `test/ablation.sh` refuses to start if it is
not, so these transcripts are reproducible by anyone running what is published here.

Each row applies its toggle **before** the boot, so the guest's own probe runs under the condition
being tested, and `nft list ruleset` is captured before and after every toggle — which is what makes
"one variable at a time" checkable instead of asserted.

| # | Control toggled | `proof` exit | Forward-drop counters | Result |
|---|---|---|---|---|
| 1 | none (baseline) | 0 | 0 | Passes — zero is the correct reading with forwarding off |
| 2 | forward chain deleted | 1 | chain absent | Detected |
| 3 | input chain deleted | 1 | 0 | Detected |
| 4 | `ip_forward=1`, guard intact | 1 | **3** | Detected — **and the drop rules fire** |
| 5 | leak-demo boot, guard up | 1 | 0 | Marker refusal fires |
| 6 | full leak — guard removed, forwarding on, NAT | 1 | chain absent | `SKIFF-OUTBOUND-ESCAPED`, detected |

Transcripts, launcher logs and all twelve ruleset captures: [`docs/ablation-2026-08-05/`](docs/ablation-2026-08-05/).

**Row 4 is the one that mattered.** With the guard fully intact and forwarding enabled, the guest's
probe traversed the forward hook and left three packets on the counters — the one piece of evidence
in this project the guest cannot author. Until this run that had only ever been shown in the
namespaced harness, never on real hardware. It is also the exact condition under which check 2b
flips from "zero is correct" to "zero is a failure", and it graded correctly in both directions.

**Row 5 corrects its own specification.** It was written to show "counters above baseline". It
cannot: `proof` stops at check 0 on the marker, and with forwarding off the counters are
structurally zero regardless. What it does show is narrower and more useful — the guest was booted
with a deliberate escape route while the guard stayed up, and with the marker removed `proof` passed,
exit 0. That verdict was *correct*; the host really was intact. The point is that **the transcript
of a deliberately weakened run is indistinguishable from a normal one**, because the transcript
describes the host and it was the run *configuration* that was abnormal. The marker is the only
thing that records the difference.

**Row 6 is not a single-variable row.** It toggles four things — guard deleted, forwarding on, NAT
added, and the guest interface exempted from firewalld — and all four were necessary. Removing
skiff's guard alone produced no egress at all, for the same reason described in section 4: two
unrelated host firewalls each independently stop this before skiff's rules are reachable. Rows 1–5
carry the single-variable claim; row 6 answers a different question, which is whether a genuine
escape is detected when one actually happens.

**Measured while the baseline row was up:** 28.2 tok/s (128 tokens, 5.56 s wall), CPU-only, in the
shipped posture.

### 4. Leak demo — DEMONSTRATED end-to-end, 2026-08-01

A proof that can only pass proves nothing. So the controls were deliberately removed on real
hardware and the guest was allowed to genuinely reach the public internet — and the question was
whether `proof` would say so.

It did, on **seven independent host-side checks, before it read a single word the guest wrote**:

```
FAIL: ip_forward is '1', want 0
FAIL: forward chain missing its iifname counter-drop rule
FAIL: forward chain missing its oifname counter-drop rule
FAIL: input chain missing the established-only counter-accept rule (design review I1 / S-7)
FAIL: input chain missing its counter-drop rule — guests could initiate to the host
FAIL: no forward-chain counter lines to read (see check 2)
FAIL: skiff-tap0 disable_ipv6 = '0', want 1 — guest→host RA injection is live
FAIL: GUEST REACHED THE OUTSIDE WORLD:
SKIFF-OUTBOUND-ESCAPED: TCP connect to 1.1.1.1:443 succeeded via 172.30.0.1 — ISOLATION BROKEN
```

That ordering is the design argument made concrete: the host kernel indicted the configuration on
its own, and the guest's own confession was the last line, not the first. Full transcript:
[`docs/leak-demo-2026-08-01.txt`](docs/leak-demo-2026-08-01.txt).

**Why the escape needed three separate things removed.** Getting a guest packet onto the internet
required disabling the host's IP forwarding guard *and* the VPN's own `forward policy drop` *and*
adding the interface to a firewalld zone, because firewalld's forward chain ends in `reject with
icmpx admin-prohibited` for any interface it does not know. Two host firewalls each independently
prevented the escape before skiff's own rules mattered. That is worth stating plainly: on a
defended workstation, several unrelated things must fail at once before this is even reachable.

**The drill-marker catch, separately.** `proof` refuses to grade a leak-demo run at all — a marker
written by the launcher hard-fails check 0 before anything else is evaluated, so a rehearsal can
never be mistaken for a passing run: [`docs/leak-demo-marker-2026-08-01.txt`](docs/leak-demo-marker-2026-08-01.txt).
The marker was removed for the primary run above, because a real misconfiguration would not have one.

**Also exercised without hardware:** `test/proof_harness.sh` runs **16 scenarios** driving the real
`proof` against real kernel state and real console-log files — one expecting exit 0, fifteen
expecting exit 1 — including a log carrying **all three** markers (`BLOCKED`, `PROBE-BROKEN`,
`ESCAPED`) where `ESCAPED` correctly wins.

```
bash test/proof_harness.sh
```

It refuses to grade a scenario whose fixture failed before `proof` ran, so a broken fixture cannot
be counted as a check that "correctly failed". It is mutation-verified the same way the Go suites
are: deleting `proof`'s `ESCAPED` branch turns the `escaped-wins` scenario red and leaves the other
fifteen green.

## Honest numbers

Measured, not estimated. Anything not yet measured says so.

| | |
|---|---|
| Test rig | bare-metal Fedora 44, Ryzen 9 3900X, 24 threads, 16 GB RAM, `/dev/kvm` present |
| Run configuration | 1 instance × 3072 MiB × 4 vCPU (`config.yaml`) |
| Hypervisor | Firecracker v1.16.1 (pinned, sha256 in `build/pins.env`) |
| Guest kernel | vmlinux 6.18.36 (Firecracker CI build, pinned) · 26.4 MiB |
| Inference server | llama.cpp commit `876a4321163249c43ca4e986818fab5ab081f282` (tag b10216), statically linked, 37 MB inside the rootfs |
| Model | Qwen3-1.7B Q4_K_M — 1,282,439,264 bytes (1.19 GiB) |
| `rootfs.ext4` | 512 MiB |
| `models.ext4` | 1.44 GiB |
| Assembled `stick/` | 3.2 GiB of files, 10 entries in `MANIFEST.sha256` (2.5 GiB on this ext4 build box — `models.ext4` is sparse; budget the full 3.2 GiB for a vfat/exFAT stick, which cannot keep holes) |
| USB drive model + copy time | **not yet measured** — the from-a-real-stick run is owner-run and still owed |

Notes on the numbers:

- **CPU-only.** Firecracker has no GPU passthrough. In the target environment that is a feature:
  there is no accelerator, no driver stack and no vendor daemon in the trust boundary.
- **The model file is carried twice** — once as `models/<name>.gguf` (which `run` existence-checks)
  and once packed inside `models.ext4` (which the guest actually mounts). That costs ~1.2 GiB of
  stick and is the obvious v0.2 trim.
- **Scaling up.** An 8B-Q4 needs roughly 5–6 GB per VM; on a 16 GB box that is about two instances.
  The launcher does not take your word for it either way — it reads `MemAvailable` from
  `/proc/meminfo` and refuses to boot `instances × ram_mib + 1024 MiB` of headroom it cannot see.

---

## Isolation model

**v0.1 — TAP + counted nftables forward-drop + established-only input.**

- Each instance gets its own TAP (`skiff-tap<N>`, `172.30.<N>.1/30`), created by `netsetup`,
  **unbridged**. A /30 point-to-point segment with exactly one guest on it means cross-instance
  frame observation is structurally impossible rather than merely firewalled — there is no shared L2
  domain to sniff.
- `net.ipv4.ip_forward = 0` is the primary control: the host never routes a guest packet anywhere.
- `table inet skiff` adds defense in depth: both directions of forward traffic dropped **with
  counters**, and input from a TAP accepted only in `ct state established` (so a guest may answer the
  host, never initiate to it — otherwise any `0.0.0.0` listener on the host is an egress proxy).
- IPv6 is removed at two layers: the guest boots with `ipv6.disable=1`, and `netsetup` sets
  `disable_ipv6=1` per TAP, so there is no RA path to the host even if the guest kernel changed.
  Per-TAP `rp_filter=1` is set too, but it is **a no-op while the host's `conf.all.rp_filter` is
  `2`** — the kernel takes the max of the two — so it is listed under
  [Limitations](#limitations-v01) rather than counted as a control.
- TAP creation is delete-then-add, never "add or ignore EEXIST": a pre-existing `skiff-tap0` left by
  a crashed run — or planted and enslaved to a bridge carrying the real NIC — would pass every
  host-side check, because bridging is L2 and never reaches the routing stage. The device is ours or
  the setup fails.

**Which evidence is authoritative.** Host-side kernel state — `ip_forward`, the per-chain rule
assertions, the drop counters, the TAP addressing, `disable_ipv6` — is the evidence. The guest's own
`SKIFF-OUTBOUND-*` console strings are **corroboration only**: they are written by the guest, so a
hostile guest can write any of them. `proof` grades markers `ESCAPED` → `PROBE-BROKEN` → `BLOCKED`,
so a log claiming both an escape and a block resolves to the escape, and a `BLOCKED` line never
carries a verdict on its own.

**v0.2 roadmap: no NIC at all.** Guests get zero network devices; host↔guest runs over virtio-vsock.
At that point isolation is a property of the device model rather than of firewall configuration —
and it is the honest answer to "how is this different from `docker run --network none`".

---

## Bundle integrity (`MANIFEST.sha256`)

The pins in `build/pins.env` protect the build box; `MANIFEST.sha256` protects the artifact in the
field. Without it, a stick whose `rootfs.ext4` was swapped in transit boots cleanly and `proof`
still passes — every check in `proof` measures the *host's* configuration, not the *bundle's*
contents. `run up` verifies the manifest before booting anything, using Go's standard-library
sha256.

| Condition | Grade |
|---|---|
| Listed file changed or missing | **FATAL** — names the file, refuses to boot |
| `MANIFEST.sha256` present but empty or unparseable | **FATAL** — a manifest that verifies nothing is not a clean bundle |
| `MANIFEST.sha256` absent | WARNING — `unverified bundle (dev tree?)`, boots |
| File present but not listed | WARNING — names the files, boots |

**`config.yaml` is deliberately excluded from the manifest.** It is user configuration, not an
integrity-critical artifact, and it is the one file this README tells you to edit — covering it would
make the documented workflow (raise `instances`, boot) report `TAMPERED` on a bundle nobody attacked,
and a gate that fires on the documented workflow is the one people learn to bypass. The cost is
bounded and worth stating plainly: an edit to `config.yaml` can change instance count, RAM, vCPUs and
the model *filename* — and nothing else. The weights the guest actually loads come from
`models.ext4`, which **is** covered, mounted read-only at `/models`, where init opens the fixed path
`/models/model.gguf`; the `model:` field only drives a host-side existence check. `bundle.sh` says
the same thing in a comment at the line that excludes it.

Two manifest entries are independently checkable against `build/pins.env` without trusting this
repository at all: `kernel/vmlinux` is `ea80af24…` and `models/qwen3-1.7b-q4.gguf` is `d2387ca2…`,
the same digests recorded as `KERNEL_SHA256` and `MODEL_SHA256` when those files were fetched from
upstream.

Provenance is unsigned — a sha256 manifest tells you the bundle is internally consistent with the
build that produced it, not who produced it. Signing is v1.

---

## Rebuilding from source

`run` is a **build artifact** committed nowhere and rebuilt from `cmd/run` + `internal/`. After any
change under `internal/` or `cmd/`:

```
go build -o run ./cmd/run
```

**before** `./run up`, or you will boot stale code. This is not hypothetical bookkeeping: it cost a
real debugging cycle here — a kernel-panic fix was already correct in `internal/vm/vm.go` and the
next boot panicked identically, because the binary on disk predated the fix by seven minutes.

Every build script exports `GOTOOLCHAIN=local`, so the Go toolchain never fetches itself over the
network mid-build. Assemble the stick with:

```
bash build/bundle.sh
```

It refuses to run if any input artifact is missing, and prints the assembled size and the manifest's
file count.

---

## Limitations (v0.1)

- SMT enabled on the test host (contra Firecracker's production host checklist)
- no jailer (v1 hardening)
- kernel `.config` not independently pinned
- no cgroup/CPU/PID limits on the VMM process (v1)
- provenance unsigned (sha256 manifest only)
- **The escape demo was run on one host, with that host's own defenses deliberately lowered.**
  It is genuine — the guest reached the public internet and `proof` failed (section 4) — but
  reaching that state required disabling the host's forwarding guard, the VPN's own forward-drop
  chain, and firewalld's default rejection. It has not been repeated across different distributions
  or firewall configurations.
- **Per-TAP `rp_filter=1` is a no-op on this host.** The kernel applies
  `max(net.ipv4.conf.all.rp_filter, net.ipv4.conf.<iface>.rp_filter)`, and this box ships
  `conf.all = 2` (loose), so the effective mode stays loose no matter what `netsetup` writes on
  the TAP. Making it strict would mean setting the host-wide `conf.all`, which changes
  reverse-path filtering for every interface on the machine — skiff deliberately does not touch a
  host-wide sysctl for a control it does not depend on. The real anti-spoofing controls here are
  the /30 point-to-point segments and the counted nftables drop rules; `netsetup`'s comment says
  the same thing at the line that sets it.
- The ablation matrix above is a **planned** experiment, not results.
- CPU-only inference — Firecracker has no GPU passthrough. In the target environment that is a
  feature, but it is a hard limit on throughput.
- `netsetup.state` guards against the *mistake* of booting with no guard up; it does not guard
  against the operator. It is root-owned where the filesystem supports it, but the bundle directory
  is user-writable, so unlink-and-recreate is available to the user running the launcher. On
  vfat/exFAT — the medium this bundle is designed to ship on — owner and mode come from the mount
  options and cannot be set at all; `netsetup` says so and continues rather than aborting with the
  host half-configured.
- `run` resolves the bundle root from the current working directory, while `netsetup` and `proof`
  `cd` to their own. Run `./run` from the bundle directory; from elsewhere it fails loud (config not
  found) rather than silently.
- One shared read-only rootfs plus a guest tmpfs, not a per-VM overlay. Nothing in v0.1 needs
  per-VM writable state; this is a deliberate deviation from the original design note.

---

## Thanks

This project started from a conversation with **Mathew Mathew** (ClaraMap), author of
[OpenClawMachines](https://github.com/mathaix/OpenClawMachines).

He took the time to walk me through what he is building and was open about how it works — the kind
of generosity that is easy to underestimate and hard to repay. He is a good guy, and it showed. The
architecture patterns here were learned from his work: Firecracker microVMs as the unit of
isolation, and one Linux box you already own as the unit of deployment rather than a cluster you
have to stand up first. That second idea is his wedge, and it is the reason this project exists in
this shape at all.

**clawcage is a lean, independent project** — no fork, no dependency, no shared code. The debt is in
the shape of the thing, and it is gladly acknowledged.

I am looking forward to contributing to the **OpenClaw community** and working with these folks
properly in the future. It is a genuinely good group of people, and worth your time if you are
building in this space.

---

## License

Apache-2.0 — full text in `LICENSE` (the canonical upstream text, unmodified; the copyright-holder
line in its appendix is filled in at publication).

The model file carries its own license and is not covered by this repository's: **Qwen3-1.7B Q4_K_M
(GGUF, ggml-org), Apache-2.0**, with the license verification recorded in
`docs/model-license-check.md`. Everything else the build scripts fetch at a pinned digest keeps its
own terms too: Firecracker (Apache-2.0), the Linux kernel image from Firecracker's CI (GPL-2.0),
llama.cpp (MIT) and BusyBox (GPL-2.0).

---

*Reviewed 5 August 2026.*
