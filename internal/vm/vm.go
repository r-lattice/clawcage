// Package vm plans and runs skiff's Firecracker microVMs: it turns a config plus
// an instance index into a fixed, testable addressing/boot plan (Plan), then
// launches a VMM against that plan (Launch) and supervises it (WaitReady, Stop).
package vm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"skiff/internal/config"
	"skiff/internal/fc"
)

// stopGrace is how long Stop waits after SIGTERM before resorting to SIGKILL.
// A var, not a const, only so tests can shorten it; nothing else may write it.
var stopGrace = 5 * time.Second

var (
	// ErrForceKilled: the VMM ignored SIGTERM for the whole grace period and had to be
	// SIGKILLed. The guest got no chance to flush, so its disk state is suspect.
	ErrForceKilled = errors.New("firecracker ignored SIGTERM and was force-killed")
	// ErrAlreadyExited: the VMM was gone before Stop asked it to leave — usually it
	// crashed, and the console log is the interesting artefact.
	ErrAlreadyExited = errors.New("firecracker had already exited")
)

type Instance struct {
	Index      int
	Socket     string
	TAP        string
	GuestIP    string
	GuestMAC   string
	BootArgs   string
	ConsoleLog string
}

type Running struct {
	Instance
	Cmd *exec.Cmd
	// console is the VMM's stdout/stderr sink. Running owns it so Stop can close it:
	// without an owner it is one leaked descriptor per instance for the life of the CLI.
	console *os.File
}

// RunDir is where the API sockets and console logs live: NEVER inside the bundle
// (S-4). The bundle runs from a USB stick, and vfat/exFAT cannot hold a unix domain
// socket at all — and their mode bits are synthesised from mount options, so a 0700
// dir there would not actually be 0700. Default /tmp/skiff-run.d; override with
// SKIFF_RUN_DIR (e.g. $XDG_RUNTIME_DIR/skiff, already 0700 and per-user).
// proof and cmd/run read the same env var with the same default.
func RunDir() string {
	if d := os.Getenv("SKIFF_RUN_DIR"); d != "" {
		return d
	}
	return "/tmp/skiff-run.d"
}

// Plan is pure. root is the bundle root (kernel/rootfs/models images); it deliberately
// does NOT contribute to the socket/console paths — see RunDir.
func Plan(cfg *config.Config, root string, index int) Instance {
	run := RunDir()
	guest := fmt.Sprintf("172.30.%d.2", index)
	host := fmt.Sprintf("172.30.%d.1", index)
	// Default: EMPTY gateway — the guest has no default route, which is half the
	// isolation story. SKIFF_LEAK_DEMO=1 deliberately sets the host as gateway so
	// the guard can be demonstrated failing (design review B2); never set it outside the
	// documented leak-demo procedure.
	gw := ""
	if os.Getenv("SKIFF_LEAK_DEMO") == "1" {
		gw = host
	}
	return Instance{
		Index:   index,
		Socket:  filepath.Join(run, fmt.Sprintf("skiff-%d.sock", index)),
		TAP:     fmt.Sprintf("skiff-tap%d", index),
		GuestIP: guest,
		// Hex verb, never the decimal one (S-10): a MAC octet is hexadecimal. The two
		// render identically for index < 10 and diverge the moment the config's
		// instances cap moves past 9.
		GuestMAC: fmt.Sprintf("06:00:AC:1E:%02x:02", index),
		// ipv6.disable=1 (S-1): ip= configures IPv4 only, so an IPv6-capable guest still
		// gets a link-local address and can send Router Advertisements at the host —
		// making the guest the host's IPv6 gateway. No stack, no class.
		// root=/dev/vda (S-9): explicit, not inferred from is_root_device.
		// init=/init is NOT optional. The image's init script lives at /init, but for a
		// DISK-backed root the kernel only searches /sbin/init, /etc/init, /bin/init,
		// /bin/sh — /init is an initramfs-only convention (rdinit=), so without this the
		// kernel finds none of the four and panics "No working init found".
		BootArgs: fmt.Sprintf(
			"console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 root=/dev/vda ro init=/init ip=%s::%s:255.255.255.252::eth0:off",
			guest, gw),
		ConsoleLog: filepath.Join(run, fmt.Sprintf("skiff-%d.console.log", index)),
	}
}

// ensureRunDir creates the run dir at 0700 AND enforces that mode on a directory that
// already existed. MkdirAll applies its mode only to directories it creates: on a
// pre-existing world-readable dir it returns nil and changes nothing. That matters
// because the path is predictable and its default parent (/tmp) is world-writable, so
// another local user can create it first — anyone who can then connect() to the API
// socket inside can PATCH a drive's backing file and PUT /snapshot/create, i.e. write
// guest memory to an arbitrary host path as this user. The directory mode is the
// control that does not rest on umask, so it is checked rather than assumed.
func ensureRunDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Lstat, not Stat: MkdirAll happily follows a symlink planted at the predictable
	// path, and a Stat-based check would then enforce 0700 on the attacker's target
	// while skiff wrote its sockets wherever the link pointed.
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("run dir %s is a symlink; refusing to use it (set SKIFF_RUN_DIR to a directory you own)", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("run dir %s exists and is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("run dir %s: cannot determine ownership", dir)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("run dir %s is owned by uid %d, not %d; refusing to use another user's directory", dir, st.Uid, uid)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		// Ours, merely loose: tighten it rather than failing the run.
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("run dir %s has mode %#o, want 0700, and chmod failed: %w", dir, perm, err)
		}
	}
	return nil
}

// killAndReap kills the VMM and waits for it, returning its exit state for the failure
// diagnosis. The Wait is not optional: a killed child that is never waited on stays a
// zombie for the life of the CLI, and its exit state — the one fact that says whether
// firecracker died of our signal or had already crashed — is otherwise lost.
func killAndReap(cmd *exec.Cmd) string {
	if cmd.Process == nil {
		return "was never started"
	}
	_ = cmd.Process.Kill()
	if err := cmd.Wait(); err != nil {
		return "exited: " + err.Error()
	}
	return "exited cleanly"
}

func Launch(ctx context.Context, root string, inst Instance, cfg *config.Config, fcBin string) (*Running, error) {
	if err := ensureRunDir(filepath.Dir(inst.Socket)); err != nil {
		return nil, fmt.Errorf("instance %d: %w", inst.Index, err)
	}
	_ = os.Remove(inst.Socket) // stale socket from a previous run: idempotent re-run
	logf, err := os.Create(inst.ConsoleLog)
	if err != nil {
		return nil, fmt.Errorf("instance %d: console log: %w", inst.Index, err)
	}
	cmd := exec.CommandContext(ctx, fcBin, "--api-sock", inst.Socket)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, fmt.Errorf("instance %d: start firecracker: %w", inst.Index, err)
	}
	// The child exists from here on, so every failure path below must reap it and
	// release the console log before returning.
	abort := func(cause error) error {
		state := killAndReap(cmd)
		if cerr := logf.Close(); cerr != nil {
			cause = errors.Join(cause, cerr)
		}
		return fmt.Errorf("%w (firecracker %s; console: %s)", cause, state, inst.ConsoleLog)
	}
	if err := waitForSocket(inst.Socket, 5*time.Second); err != nil {
		return nil, abort(fmt.Errorf("instance %d: %w", inst.Index, err))
	}
	c := fc.New(inst.Socket)
	steps := []struct {
		name string
		fn   func() error
	}{
		{"machine-config", func() error { return c.MachineConfig(cfg.VCPUs, cfg.RAMMiB) }},
		{"boot-source", func() error { return c.BootSource(filepath.Join(root, "kernel/vmlinux"), inst.BootArgs) }},
		// ORDER IS A CONTRACT (S-9). Two things, both load-bearing:
		//  1. The root drive's id MUST be exactly "rootfs" — fc.Drive infers
		//     is_root_device from `id == "rootfs"`. Rename it and the guest has no root.
		//  2. The rootfs drive MUST be PUT before the models drive: Firecracker assigns
		//     virtio block devices in PUT order, so rootfs = /dev/vda (matching the
		//     explicit root= in BootArgs) and models = /dev/vdb (what the guest init
		//     mounts). Swap these two lines and the VM boots the model image as root.
		{"rootfs drive", func() error { return c.Drive("rootfs", filepath.Join(root, "rootfs/rootfs.ext4"), true) }},
		{"models drive", func() error { return c.Drive("models", filepath.Join(root, "models.ext4"), true) }},
		{"net iface", func() error { return c.NetIface("eth0", inst.TAP, inst.GuestMAC) }},
		{"start", c.Start},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			return nil, abort(fmt.Errorf("instance %d %s: %w", inst.Index, s.name, err))
		}
	}
	return &Running{Instance: inst, Cmd: cmd, console: logf}, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("firecracker API socket %s never appeared", path)
}

func (r *Running) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:8080/health", r.GuestIP)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("instance %d: llama-server at %s not ready after %s (console: %s)",
		r.Index, url, timeout, r.ConsoleLog)
}

// Stop shuts the VMM down: SIGTERM, then SIGKILL after stopGrace. It reports what
// actually happened instead of swallowing it — a caller tearing down four instances
// needs to tell a clean shutdown from a force-kill (guest state suspect) from a VMM
// that had already died (it crashed; read the console log):
//
//	clean exit, or death by our own SIGTERM  -> nil
//	had to SIGKILL after the grace period    -> error matching ErrForceKilled
//	already gone when asked                  -> error matching ErrAlreadyExited
//	any other non-zero exit                  -> error describing the exit state
//
// The console log and the API socket are released on every path, and failures to
// release them are joined into the returned error rather than dropped.
func (r *Running) Stop() error {
	var problems []error
	if r.Cmd != nil && r.Cmd.Process != nil {
		problems = append(problems, r.terminate())
	}
	if r.console != nil {
		if err := r.console.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			problems = append(problems, fmt.Errorf("instance %d: close console log %s: %w", r.Index, r.ConsoleLog, err))
		}
	}
	// A socket that is already gone is the normal case, not a problem.
	if err := os.Remove(r.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, fmt.Errorf("instance %d: remove api socket %s: %w", r.Index, r.Socket, err))
	}
	return errors.Join(problems...) // nil when every entry is nil
}

func (r *Running) terminate() error {
	// SIGTERM, not SIGINT: the interface contract is SIGTERM, and firecracker's own
	// signal handling treats it as the orderly-shutdown request.
	if err := r.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("instance %d: %w", r.Index, ErrAlreadyExited)
		}
		return fmt.Errorf("instance %d: SIGTERM: %w", r.Index, err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Cmd.Wait() }()
	select {
	case err := <-done:
		return r.exitState(err, false)
	case <-time.After(stopGrace):
		_ = r.Cmd.Process.Kill()
		return r.exitState(<-done, true)
	}
}

func (r *Running) exitState(waitErr error, forced bool) error {
	if forced {
		return fmt.Errorf("instance %d: %w after %s (console: %s)", r.Index, ErrForceKilled, stopGrace, r.ConsoleLog)
	}
	if waitErr == nil {
		return nil
	}
	// A VMM with no SIGTERM handler is killed BY the signal we just sent, so Wait
	// reports "signal: terminated". That is the shutdown succeeding, not failing.
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM {
			return nil
		}
	}
	return fmt.Errorf("instance %d: firecracker exited uncleanly: %w (console: %s)", r.Index, waitErr, r.ConsoleLog)
}
