package vm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- fake firecracker -------------------------------------------------------
//
// Launch needs a real child process, and the brief rules out a shell stub. This is
// Go's standard helper-process idiom: the test binary re-executes ITSELF as the
// "firecracker" binary. TestMain dispatches on SKIFF_FAKE_FC before m.Run() parses
// flags, so the `--api-sock <path>` argv Launch passes never reaches the flag parser.
// No real VM assets are needed — Launch only ever hands the asset paths to the API as
// strings, so a fake that answers 204 exercises the whole lifecycle.

func TestMain(m *testing.M) {
	if os.Getenv("SKIFF_FAKE_FC") != "" {
		fakeFirecrackerMain()
		return
	}
	os.Exit(m.Run())
}

func fakeFirecrackerMain() {
	var sock string
	for i, a := range os.Args {
		if a == "--api-sock" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	mode := os.Getenv("SKIFF_FAKE_FC")

	// SIGINT is ignored in EVERY mode. Stop's contract is SIGTERM; a stub that also
	// died on SIGINT could not tell the two signals apart, and the finding being
	// tested is precisely that the old code sent os.Interrupt.
	signal.Ignore(syscall.SIGINT)

	if mode == "deaf" {
		// A plain FILE where the API socket belongs: waitForSocket is satisfied, but the
		// first PUT fails immediately (connect on a non-socket) rather than after a
		// timeout — this is the fast Launch-error path.
		f, err := os.Create(sock)
		if err != nil {
			os.Exit(3)
		}
		f.Close()
	} else {
		l, err := net.Listen("unix", sock)
		if err != nil {
			os.Exit(3)
		}
		go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}

	switch mode {
	case "stubborn":
		signal.Ignore(syscall.SIGTERM) // must be force-killed after the grace period
	case "sigdefault":
		// SIGTERM left at its default disposition: dies BY the signal, which is how a
		// real firecracker with no handler exits. Stop must call that a clean stop.
	default:
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		go func() {
			<-ch
			if marker := os.Getenv("SKIFF_FAKE_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("SIGTERM"), 0o600)
			}
			os.Exit(0)
		}()
	}
	time.Sleep(60 * time.Second)
	os.Exit(9) // only reached if the test never stopped us
}

// fakeFC points Launch at the test binary in the given stub mode.
func fakeFC(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKIFF_FAKE_FC", mode)
	return exe
}

// --- process/fd inspection helpers -----------------------------------------

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("this test needs /proc: %v", err)
	}
	return len(ents)
}

// waitFDsBackTo allows for the fc client's idle keep-alive connection, which the
// kernel tears down asynchronously once the VMM process dies. The console log is a
// regular file and would never come back on its own — that is the leak under test.
func waitFDsBackTo(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := openFDs(t)
		if n <= base {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("open fds = %d, want <= %d — a file descriptor is not being closed", n, base)
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// zombieChildren counts un-reaped children of this process: a killed child that was
// never Wait()ed stays in state Z for the life of the parent.
func zombieChildren(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("this test needs /proc: %v", err)
	}
	me := strconv.Itoa(os.Getpid())
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // exited between ReadDir and ReadFile
		}
		// comm can contain spaces and parens, so parse after the LAST ')'.
		s := string(b)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) >= 2 && f[0] == "Z" && f[1] == me {
			n++
		}
	}
	return n
}

// --- run dir mode enforcement (finding 5) -----------------------------------

func TestEnsureRunDir_TightensPreExistingLooseMode(t *testing.T) {
	// The run dir sits at a PREDICTABLE path under a world-writable /tmp, so it can
	// already exist when skiff starts. MkdirAll applies its mode only when it creates
	// the directory — on a pre-existing 0777 dir it returns nil and changes nothing,
	// which would make the "the directory mode is the control" comment a lie.
	dir := filepath.Join(t.TempDir(), "skiff-run.d")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // defeat umask
		t.Fatal(err)
	}
	if err := ensureRunDir(dir); err != nil {
		t.Fatalf("ensureRunDir on a dir we own must tighten it, not fail: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("run dir mode = %#o, want 0700 — a pre-existing loose dir was left loose", perm)
	}
}

func TestEnsureRunDir_CreatesAt0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skiff-run.d")
	if err := ensureRunDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("run dir mode = %#o, want 0700", perm)
	}
}

func TestEnsureRunDir_RefusesSymlink(t *testing.T) {
	// A symlink planted at the predictable path is the /tmp attack: MkdirAll follows it
	// and succeeds, and a Stat-based mode check would then "enforce" 0700 on the
	// attacker's target instead of noticing the redirection.
	tmp := t.TempDir()
	target := filepath.Join(tmp, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "skiff-run.d")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := ensureRunDir(link)
	if err == nil {
		t.Fatal("a symlink at the run-dir path must be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should name the symlink, got: %v", err)
	}
}

// --- Launch failure paths (findings 3, 4, 6) --------------------------------

func TestLaunch_EarlyErrorsNameTheInstance(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(tmp, "skiff-run.d")
	if err := os.Symlink(target, run); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKIFF_RUN_DIR", run)

	inst := Plan(cfg(), "/bundle", 3)
	_, err := Launch(context.Background(), "/bundle", inst, cfg(), "/nonexistent/firecracker")
	if err == nil {
		t.Fatal("want error when the run dir is unusable, got nil")
	}
	if !strings.Contains(err.Error(), "instance 3") {
		t.Errorf("early Launch errors must name the instance like the steps loop does, got: %v", err)
	}
}

func TestLaunch_APIFailureReapsChildAndClosesConsoleLog(t *testing.T) {
	t.Setenv("SKIFF_RUN_DIR", t.TempDir())
	bin := fakeFC(t, "deaf")

	base := openFDs(t)
	inst := Plan(cfg(), "/bundle", 0)
	r, err := Launch(context.Background(), "/bundle", inst, cfg(), bin)
	if err == nil {
		_ = r.Stop()
		t.Fatal("want error when the API socket is not served, got nil")
	}
	if !strings.Contains(err.Error(), "instance 0 machine-config") {
		t.Errorf("error should name the instance and the failing step, got: %v", err)
	}
	// The failure diagnosis must carry the child's exit state and the console log path.
	if !strings.Contains(err.Error(), "console") {
		t.Errorf("error should point at the console log, got: %v", err)
	}
	// Poll: without a Wait() after Kill the child turns into a zombie within ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if z := zombieChildren(t); z > 0 {
			t.Fatalf("Launch left %d un-reaped child process(es) — Kill without Wait", z)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFDsBackTo(t, base)
}

// --- Stop semantics (findings 1, 2, 3) --------------------------------------

func launchFake(t *testing.T, mode string) *Running {
	t.Helper()
	t.Setenv("SKIFF_RUN_DIR", t.TempDir())
	bin := fakeFC(t, mode)
	inst := Plan(cfg(), "/bundle", 0)
	r, err := Launch(context.Background(), "/bundle", inst, cfg(), bin)
	if err != nil {
		t.Fatalf("Launch against the fake firecracker: %v", err)
	}
	return r
}

func TestStop_UsesSIGTERMAndReleasesEverything(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sigterm-received")
	t.Setenv("SKIFF_FAKE_MARKER", marker)

	base := openFDs(t)
	r := launchFake(t, "serve")
	if r.console == nil {
		t.Fatal("Running must retain the console log file so Stop can close it")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop on a clean shutdown must return nil, got: %v", err)
	}
	// The stub ignores SIGINT and only writes the marker on SIGTERM, so this fails if
	// Stop sends os.Interrupt.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("stub never received SIGTERM — Stop must not send SIGINT: %v", err)
	}
	if _, err := r.console.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("console log still open after Stop, write err = %v", err)
	}
	if _, err := os.Stat(r.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("API socket %s not removed by Stop: %v", r.Socket, err)
	}
	waitFDsBackTo(t, base)
}

func TestStop_DeathBySIGTERMIsCleanNotAnError(t *testing.T) {
	// A VMM with no SIGTERM handler is killed BY the signal, so Wait reports
	// "signal: terminated". That is the expected shutdown, not a failure — reporting it
	// as an error would make every normal `run down` look broken.
	r := launchFake(t, "sigdefault")
	if err := r.Stop(); err != nil {
		t.Fatalf("death by our own SIGTERM must be a clean stop, got: %v", err)
	}
}

func TestStop_ForceKillIsReportedNotSwallowed(t *testing.T) {
	old := stopGrace
	stopGrace = 250 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })

	r := launchFake(t, "stubborn")
	start := time.Now()
	err := r.Stop()
	if err == nil {
		t.Fatal("a VMM that ignored SIGTERM and had to be killed must not report success")
	}
	if !errors.Is(err, ErrForceKilled) {
		t.Errorf("want an error matching ErrForceKilled so callers can distinguish it, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < stopGrace {
		t.Errorf("Stop killed after %s, before the %s grace period elapsed", elapsed, stopGrace)
	}
	// Even on the force-kill path the resources are released.
	if _, err := r.console.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("console log still open after a force-kill Stop, write err = %v", err)
	}
}

func TestStop_AlreadyExitedIsDistinguishable(t *testing.T) {
	r := launchFake(t, "serve")
	if err := r.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// Second Stop: the process is gone and already reaped. The caller must be able to
	// tell this from a clean shutdown it actually performed.
	err := r.Stop()
	if !errors.Is(err, ErrAlreadyExited) {
		t.Errorf("want an error matching ErrAlreadyExited, got: %v", err)
	}
}

func TestStop_NeverStartedIsSafe(t *testing.T) {
	r := &Running{Instance: Plan(cfg(), "/bundle", 0), Cmd: exec.Command("/nonexistent")}
	if err := r.Stop(); err != nil {
		t.Errorf("Stop on a Running that was never started must not error, got: %v", err)
	}
}
