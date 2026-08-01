// cmd/run/main.go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"skiff/internal/assets"
	"skiff/internal/config"
	"skiff/internal/vm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "skiff:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: run up|down|status")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		return err
	}
	switch os.Args[1] {
	case "up":
		return up(root, cfg)
	case "status":
		return status(cfg)
	case "down":
		fmt.Println("down: stop with Ctrl-C on the 'up' process, then: sudo ./netsetup down", cfg.Instances)
		return nil
	default:
		return fmt.Errorf("unknown command %q (up|down|status)", os.Args[1])
	}
}

// up's error result is NAMED so the teardown defer can tell a failed run from a clean
// Ctrl-C and print the console logs it is about to delete.
func up(root string, cfg *config.Config) (err error) {
	if err := assets.Validate(root, cfg.Model); err != nil {
		return err
	}
	if err := checkMemory(cfg); err != nil {
		return err
	}
	if err := checkNetsetupState(root, cfg.Instances); err != nil {
		return err
	}
	if err := verifyManifest(root); err != nil {
		return err
	}
	if os.Getenv("SKIFF_LEAK_DEMO") == "1" {
		// S-6: leak-demo mode weakens isolation on purpose, and an env var can be
		// inherited, exported in a profile, or simply left set from the Step 5b run.
		// Drop a marker in the run dir BEFORE booting so ./proof refuses to grade the
		// transcript — otherwise a leaky run produces output indistinguishable from
		// a real one and could be published in the README.
		if err := os.MkdirAll(vm.RunDir(), 0o700); err != nil {
			return err
		}
		marker := filepath.Join(vm.RunDir(), "leak-demo.marker")
		body := []byte("SKIFF_LEAK_DEMO=1 — isolation deliberately weakened; this is never a real run\n")
		if err := os.WriteFile(marker, body, 0o644); err != nil {
			return err
		}
		fmt.Println("!! SKIFF_LEAK_DEMO=1 — the guest is being given a default route ON PURPOSE.")
		fmt.Println("!! wrote", marker, "— ./proof will refuse to grade this run.")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// vmCtx is deliberately NOT ctx. vm.Launch runs firecracker under
	// exec.CommandContext, whose watcher SIGKILLs the child the moment its context is
	// done — so handing it ctx would make Ctrl-C kill every VMM outright, racing the
	// orderly SIGTERM in vm.Stop. Stop would then report ErrAlreadyExited or "exited
	// uncleanly: signal: killed" on every normal shutdown. Those sentinels exist to
	// tell a crash from a clean stop, and a teardown that cries wolf on every run is
	// one nobody reads. The defer below always calls Stop, so nothing is orphaned.
	vmCtx := context.Background()
	fcBin := filepath.Join(root, "bin/firecracker")
	var running []*vm.Running
	defer func() {
		for _, r := range running {
			// Never discarded: vm.Stop distinguishes a clean shutdown from a force-kill
			// (guest never flushed) from a VMM that had already died (it crashed).
			if serr := r.Stop(); serr != nil {
				fmt.Fprintln(os.Stderr, "skiff: teardown:", serr)
			}
		}
		if err != nil {
			// The wipe below destroys console logs — including the one this function's
			// own error message just told the operator to go read. On the failure path
			// the tail goes into the terminal transcript first. Stop has already run,
			// so the logs are complete and closed.
			for _, r := range running {
				dumpConsole(r.Index, r.ConsoleLog)
			}
		}
		// Ephemeral by contract (spec §4b, design review I2): sockets and console logs
		// do not survive shutdown. proof reads console logs DURING the run.
		// The run dir is $SKIFF_RUN_DIR (default /tmp/skiff-run.d), never the
		// bundle — see vm.RunDir and S-4.
		if rerr := os.RemoveAll(vm.RunDir()); rerr != nil {
			fmt.Fprintf(os.Stderr,
				"skiff: run dir %s not fully removed: %v — sockets and console logs from this run may have survived it\n",
				vm.RunDir(), rerr)
		}
	}()
	for n := 0; n < cfg.Instances; n++ {
		inst := vm.Plan(cfg, root, n)
		r, err := vm.Launch(vmCtx, root, inst, cfg, fcBin)
		if err != nil {
			// This instance never reaches `running`, so the teardown defer cannot dump
			// its console log — and vm.Launch's error names that exact file ("console:
			// …/skiff-N.console.log") moments before the run-dir wipe deletes it. Dump
			// it here. Launch has already reaped firecracker and closed the log on its
			// abort path, so what is on disk is complete.
			dumpConsole(inst.Index, inst.ConsoleLog)
			return err
		}
		running = append(running, r)
		fmt.Printf("instance %d: booting (console: %s)\n", n, inst.ConsoleLog)
	}
	for _, r := range running {
		if err := r.WaitReady(120 * time.Second); err != nil {
			return err
		}
		fmt.Printf("instance %d: READY — http://%s:8080\n", r.Index, r.GuestIP)
	}
	fmt.Println("all instances up — Ctrl-C to stop, ./proof", cfg.Instances, "to verify isolation")
	<-ctx.Done()
	fmt.Println("\nstopping…")
	return nil
}

// checkNetsetupState refuses to boot unless netsetup has actually run (S-2).
// The guard is a NON-PERSISTENT nftables table on a host where firewalld and other
// daemons rewrite firewall state, and "a TAP named skiff-tap0 exists" is not
// equivalent to "the guard exists" — Step 5b proves that itself by hand-creating a
// TAP with no table. netsetup writes this file as root at the bundle root and the
// launcher only reads it, which makes it a guard against the MISTAKE of booting with
// no guard up. It is NOT a control against a hostile local user: the bundle directory
// is user-writable, so whoever can run the launcher can unlink this file and write
// their own count= line, and on vfat/exFAT it cannot be root-owned at all. Stated the
// same way in the README's Limitations.
func checkNetsetupState(root string, instances int) error {
	path := filepath.Join(root, "netsetup.state")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf(
			"no netsetup state at %s — the nftables guard is not up, so nothing would stop guest egress.\n"+
				"Run this first (its own terminal is better; see S-3):\n"+
				"    sudo ./netsetup up %d $USER\n"+
				"    sudo -k",
			path, instances)
	}
	count, found := 0, false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "count=") {
			continue
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "count=")))
		if convErr != nil {
			return fmt.Errorf("%s: unparseable count line %q — re-run: sudo ./netsetup up %d $USER", path, line, instances)
		}
		// netsetup writes a canonical 0..256 integer and nothing else (its own regex
		// guard, task-5 F-1), so a negative value means the file was hand-written or
		// corrupted. Name THAT. Falling through to the shortfall branch below would
		// report "netsetup covers -5 instance(s)" as if netsetup had written it, and a
		// missing count= line would report identically — two different faults, one
		// message, neither actionable.
		if n < 0 {
			return fmt.Errorf("%s: count=%d is not a valid instance count — re-run: sudo ./netsetup up %d $USER", path, n, instances)
		}
		count, found = n, true
	}
	if !found {
		return fmt.Errorf("%s: no count= line — re-run: sudo ./netsetup up %d $USER", path, instances)
	}
	if count < instances {
		return fmt.Errorf(
			"netsetup covers %d instance(s) but config.yaml asks for %d — instance(s) %d..%d would boot onto TAPs that do not exist or are not guarded.\n"+
				"Run: sudo ./netsetup up %d $USER",
			count, instances, count, instances-1, instances)
	}
	return nil
}

// verifyManifest checks every file listed in MANIFEST.sha256 against its recorded
// digest (X-1). pins.env protects the BUILD BOX; this protects the artifact in the
// field. A stick whose rootfs.ext4 was swapped in transit passes every host-side
// check in proof, because proof measures the HOST's configuration, not the BUNDLE's
// contents — for a project whose pitch is "verify me instead of trusting me", that
// is the gap that matters most after the evidence layer itself.
// Format is sha256sum's own output: "<64 hex><two spaces><path relative to root>".
func verifyManifest(root string) error {
	path := filepath.Join(root, "MANIFEST.sha256")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("WARNING: no MANIFEST.sha256 — unverified bundle (dev tree?)")
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var problems []error
	checked := 0
	listed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			problems = append(problems, fmt.Errorf("unparseable manifest line: %q", line))
			continue
		}
		want := fields[0]
		rel := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		listed[rel] = true
		f, openErr := os.Open(filepath.Join(root, rel))
		if openErr != nil {
			problems = append(problems, fmt.Errorf("listed in manifest but unreadable: %s", rel))
			continue
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		f.Close()
		if copyErr != nil {
			problems = append(problems, fmt.Errorf("%s: read failed: %w", rel, copyErr))
			continue
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			problems = append(problems, fmt.Errorf("TAMPERED: %s\n    manifest: %s\n    actual:   %s", rel, want, got))
		}
		checked++
	}
	if len(problems) > 0 {
		return fmt.Errorf("bundle integrity check FAILED against %s:\n%w", path, errors.Join(problems...))
	}
	// An empty (or all-blank) manifest is the cheapest tamper there is: no digest has
	// to be forged, the loop simply has nothing to check, and the gate would otherwise
	// print "bundle verified: 0 file(s)" and boot. A manifest that exists and lists
	// nothing is a broken manifest, never a clean bundle. (Absent is different — that
	// is the dev tree, handled above with a warning.)
	if checked == 0 {
		return fmt.Errorf("%s exists but lists no files — truncated or replaced; it verifies nothing, so the bundle is unverifiable", path)
	}
	fmt.Printf("bundle verified: %d file(s) match MANIFEST.sha256\n", checked)
	reportUnlisted(root, listed)
	return nil
}

// alwaysUnlisted are the bundle-root files no correct MANIFEST.sha256 can ever list:
// bundle.sh excludes the manifest from its own listing, and netsetup.state is written
// at the bundle root by `sudo ./netsetup up` on whatever machine runs the stick — it
// exists on every correct run, by construction, after the manifest was built.
//
// config.yaml joins them deliberately (Task 9). It is the one file the README tells the
// operator to edit, so covering it would make the documented workflow — raise `instances`,
// boot — report `TAMPERED` on a bundle nobody attacked, and a gate that fires on the
// documented workflow is the one people learn to bypass. Warning about it on every correct
// run would be the same disease, milder. What that costs is bounded and worth stating: an
// edit to config.yaml can change instance count, RAM, vCPUs and the model FILENAME, and
// nothing else — the bytes the guest actually loads come from models.ext4 (manifest-covered,
// mounted read-only at /models, where init opens the fixed path /models/model.gguf), so the
// `model:` field selects only a host-side existence check, never the weights.
var alwaysUnlisted = map[string]bool{
	"MANIFEST.sha256": true,
	"netsetup.state":  true,
	"config.yaml":     true,
}

// unlistedFiles returns every file under root that MANIFEST.sha256 does not cover,
// sorted so two runs over the same bundle print the same thing. bundle.sh builds the
// manifest from `find . -type f` over the whole stick, so the listing is a complete
// census and anything else was added after the build.
func unlistedFiles(root string, listed map[string]bool) ([]string, error) {
	var extra []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if listed[rel] || alwaysUnlisted[rel] {
			return nil
		}
		extra = append(extra, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(extra)
	return extra, nil
}

// maxUnlistedShown caps the warning: a stick that has been plugged into a machine
// which writes a trash directory can carry hundreds of stray files, and a wall of
// them would bury the line above it that actually matters.
const maxUnlistedShown = 10

// reportUnlisted names files the manifest does not cover. WARNING, never a refusal,
// and that asymmetry is deliberate:
//   - listed-but-missing and listed-but-changed are FATAL, because both alter
//     something `run up` actually loads (kernel, rootfs, models.ext4 — and netsetup
//     and proof, the two things the operator runs as root).
//   - present-but-unlisted cannot reach the boot path: every path the launcher opens is
//     fixed in code, and the one indirection — config.yaml's model: field — resolves to
//     an existence check on the host (assets.Validate), not to anything loaded: the guest
//     mounts manifest-covered models.ext4 and opens the fixed path /models/model.gguf
//     inside it. So a smuggled .gguf dropped next to the real one is never read by
//     anything. What remains is a file a human might be tricked into running, which is
//     answered by naming it, not by refusing to boot.
//
// Refusing here would also break every correct run: netsetup.state (handled above) and
// anything another OS left on the stick would each be a hard stop.
func reportUnlisted(root string, listed map[string]bool) {
	extra, err := unlistedFiles(root, listed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not scan %s for files missing from MANIFEST.sha256: %v\n", root, err)
		return
	}
	if len(extra) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"WARNING: %d file(s) are on the bundle but NOT listed in MANIFEST.sha256 — the launcher\n"+
			"         loads none of them, but the build did not put them here either:\n", len(extra))
	for i, rel := range extra {
		if i == maxUnlistedShown {
			fmt.Fprintf(os.Stderr, "         … and %d more\n", len(extra)-maxUnlistedShown)
			break
		}
		fmt.Fprintln(os.Stderr, "         "+rel)
	}
}

// consoleTailLines is how many trailing console lines the failure path prints: enough
// to carry a kernel panic or firecracker's own refusal, short enough to stay readable.
const consoleTailLines = 40

// dumpConsole puts the end of a console log in the terminal transcript, because the
// file itself is about to be deleted by the run-dir wipe.
func dumpConsole(index int, path string) {
	lines, err := tailFile(path, consoleTailLines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skiff: instance %d: console log %s could not be read: %v\n", index, path, err)
		return
	}
	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "skiff: instance %d: console log %s is EMPTY — firecracker wrote nothing at all\n", index, path)
		return
	}
	fmt.Fprintf(os.Stderr, "skiff: instance %d: last %d line(s) of %s (the run dir is wiped on exit):\n", index, len(lines), path)
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, "    "+l)
	}
}

// consoleTailBytes bounds how much of a console log's end tailFile reads. A guest can
// write to ttyS0 without limit, and this runs on the failure path — where the host may
// already be out of the memory that made the run fail.
const consoleTailBytes = 64 << 10

// tailFile returns up to maxLines from the end of path.
func tailFile(path string, maxLines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if fi.Size() > consoleTailBytes {
		start = fi.Size() - consoleTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		return nil, nil // an empty console log is its own diagnosis, not a blank line
	}
	lines := strings.Split(text, "\n")
	if start > 0 && len(lines) > 1 {
		lines = lines[1:] // the window opened mid-line; that fragment is not a line
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

// checkMemory refuses to overcommit the host (design review I5): instances × ram_mib
// plus a 1 GiB host margin must fit in MemAvailable, or we fail loud before
// any VM boots — this box has an OOM history.
func checkMemory(cfg *config.Config) error {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return fmt.Errorf("parse MemAvailable: %w", err)
		}
		need := cfg.Instances*cfg.RAMMiB + 1024
		if avail := kb / 1024; avail < need {
			return fmt.Errorf(
				"not enough memory: need %d MiB (%d × %d ram_mib + 1024 host margin), MemAvailable is %d MiB — lower instances or ram_mib in config.yaml",
				need, cfg.Instances, cfg.RAMMiB, avail)
		}
		return nil
	}
	return fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

func status(cfg *config.Config) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for n := 0; n < cfg.Instances; n++ {
		url := fmt.Sprintf("http://172.30.%d.2:8080/health", n)
		if resp, err := client.Get(url); err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			fmt.Printf("instance %d: UP (%s)\n", n, url)
		} else {
			fmt.Printf("instance %d: down\n", n)
		}
	}
	return nil
}
