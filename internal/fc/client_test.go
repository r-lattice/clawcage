package fc

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct{ Method, Path, Body string }

func fakeFC(t *testing.T) (string, *[]call) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fc.sock")
	var mu sync.Mutex
	calls := &[]call{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*calls = append(*calls, call{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	// NewUnstartedServer already bound a TCP listener; drop it before swapping
	// in the unix socket so it does not leak for the life of the test binary.
	srv.Listener.Close()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return sock, calls
}

func TestClient_SendsCorrectPUTs(t *testing.T) {
	sock, calls := fakeFC(t)
	c := New(sock)
	if err := c.MachineConfig(4, 6144); err != nil {
		t.Fatal(err)
	}
	if err := c.Drive("rootfs", "/b/rootfs.ext4", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(*calls))
	}
	mc := (*calls)[0]
	if mc.Method != "PUT" || mc.Path != "/machine-config" {
		t.Fatalf("bad call: %+v", mc)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(mc.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["vcpu_count"].(float64) != 4 || body["mem_size_mib"].(float64) != 6144 {
		t.Fatalf("bad machine-config body: %s", mc.Body)
	}
	if (*calls)[1].Path != "/drives/rootfs" {
		t.Fatalf("bad drive path: %+v", (*calls)[1])
	}
	if (*calls)[2].Path != "/actions" {
		t.Fatalf("bad start path: %+v", (*calls)[2])
	}
}

// decode is a body-field helper: every configuration call must ship a JSON
// object, so a body that will not decode is itself a failure.
func decode(t *testing.T, c call) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
		t.Fatalf("call %s %s: body is not JSON: %q: %v", c.Method, c.Path, c.Body, err)
	}
	return body
}

// TestClient_EveryEndpoint covers the whole surface Task 4 consumes and asserts
// method, path AND body fields on every call.
func TestClient_EveryEndpoint(t *testing.T) {
	sock, calls := fakeFC(t)
	c := New(sock)
	if err := c.MachineConfig(2, 4096); err != nil {
		t.Fatal(err)
	}
	if err := c.BootSource("/b/vmlinux", "console=ttyS0 reboot=k panic=1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Drive("rootfs", "/b/rootfs.ext4", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Drive("models", "/b/models.ext4", true); err != nil {
		t.Fatal(err)
	}
	if err := c.NetIface("eth0", "sk0", "AA:FC:00:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 6 {
		t.Fatalf("want 6 calls, got %d", len(*calls))
	}
	for i, got := range *calls {
		if got.Method != "PUT" {
			t.Errorf("call %d: want method PUT, got %s (%+v)", i, got.Method, got)
		}
	}

	wantPaths := []string{
		"/machine-config", "/boot-source", "/drives/rootfs",
		"/drives/models", "/network-interfaces/eth0", "/actions",
	}
	for i, want := range wantPaths {
		if got := (*calls)[i].Path; got != want {
			t.Errorf("call %d: want path %s, got %s", i, want, got)
		}
	}

	if body := decode(t, (*calls)[0]); body["vcpu_count"] != float64(2) || body["mem_size_mib"] != float64(4096) {
		t.Errorf("bad machine-config body: %s", (*calls)[0].Body)
	}
	if body := decode(t, (*calls)[1]); body["kernel_image_path"] != "/b/vmlinux" ||
		body["boot_args"] != "console=ttyS0 reboot=k panic=1" {
		t.Errorf("bad boot-source body: %s", (*calls)[1].Body)
	}
	// rootfs is the root device; every other drive is not.
	if body := decode(t, (*calls)[2]); body["drive_id"] != "rootfs" ||
		body["path_on_host"] != "/b/rootfs.ext4" ||
		body["is_root_device"] != true || body["is_read_only"] != true {
		t.Errorf("bad rootfs drive body: %s", (*calls)[2].Body)
	}
	if body := decode(t, (*calls)[3]); body["drive_id"] != "models" ||
		body["path_on_host"] != "/b/models.ext4" ||
		body["is_root_device"] != false || body["is_read_only"] != true {
		t.Errorf("bad models drive body: %s", (*calls)[3].Body)
	}
	if body := decode(t, (*calls)[4]); body["iface_id"] != "eth0" ||
		body["host_dev_name"] != "sk0" || body["guest_mac"] != "AA:FC:00:00:00:01" {
		t.Errorf("bad network-interface body: %s", (*calls)[4].Body)
	}
	if body := decode(t, (*calls)[5]); body["action_type"] != "InstanceStart" {
		t.Errorf("bad actions body: %s", (*calls)[5].Body)
	}
}

// TestClient_ErrorsOnAPIFailure: a Firecracker refusal must surface, not pass silently.
func TestClient_ErrorsOnAPIFailure(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"fault_message":"nope"}`)
	}))
	srv.Listener.Close()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	err = New(sock).Start()
	if err == nil {
		t.Fatal("want error on 400 response, got nil")
	}
	for _, want := range []string{"/actions", "400", "nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestClient_ErrorsWhenSocketMissing: no VMM listening is an error, not a no-op.
func TestClient_ErrorsWhenSocketMissing(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if err := New(sock).MachineConfig(1, 512); err == nil {
		t.Fatal("want error when no socket exists, got nil")
	}
}

// TestClient_TimesOutOnWedgedVMM: a Firecracker process that accepted the connection
// but never answers must not hang `run up` forever — vm.Launch is the first real
// caller and has no deadline of its own to fall back on.
func TestClient_TimesOutOnWedgedVMM(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleeps far past the client's deadline but not so long that srv.Close (which
		// waits for outstanding requests) drags the test out.
		time.Sleep(2 * time.Second)
	}))
	// NewUnstartedServer already bound a TCP listener; drop it before swapping in the
	// unix socket so it does not leak for the life of the test binary.
	srv.Listener.Close()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	c := New(sock)
	// Assert the deadline comes from New() itself: without this the test would pass
	// on a client whose Timeout is only ever set below, which is exactly the bug.
	if c.http.Timeout == 0 {
		t.Fatal("New() must set an http.Client Timeout — a wedged VMM would block forever")
	}
	c.http.Timeout = 50 * time.Millisecond // same field New() sets, shortened for speed
	done := make(chan error, 1)
	go func() { done <- c.Start() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wedged VMM must produce an error, not a nil return")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("client blocked with no deadline — Timeout is not wired into New()")
	}
}
