package vm

import (
	"strings"
	"testing"

	"skiff/internal/config"
)

func cfg() *config.Config {
	return &config.Config{Instances: 2, RAMMiB: 6144, VCPUs: 4, Model: "q.gguf"}
}

func TestPlan_AddressingContract(t *testing.T) {
	i := Plan(cfg(), "/bundle", 1)
	if i.TAP != "skiff-tap1" {
		t.Errorf("TAP = %q", i.TAP)
	}
	if i.GuestIP != "172.30.1.2" {
		t.Errorf("GuestIP = %q", i.GuestIP)
	}
	if i.GuestMAC != "06:00:AC:1E:01:02" {
		t.Errorf("GuestMAC = %q", i.GuestMAC)
	}
	if !strings.Contains(i.Socket, "skiff-1.sock") {
		t.Errorf("Socket = %q", i.Socket)
	}
}

// TestPlan_MACIsHexNotDecimal pins the S-10 verb. Every other MAC assertion here uses
// an index below 10, where %02x and %02d render identically — so none of them can fail
// on a decimal verb. Index 10 is deliberately above the current config cap of 8: the
// point is that raising that cap must not silently start minting invalid MAC octets
// (a decimal verb yields "10" for index 10, then collides with index 16's hex "10").
func TestPlan_MACIsHexNotDecimal(t *testing.T) {
	i := Plan(cfg(), "/bundle", 10)
	if i.GuestMAC != "06:00:AC:1E:0a:02" {
		t.Errorf("GuestMAC = %q, want 06:00:AC:1E:0a:02 — a MAC octet is hexadecimal (S-10)", i.GuestMAC)
	}
}

func TestPlan_BootArgsHaveNoGateway(t *testing.T) {
	i := Plan(cfg(), "/bundle", 0)
	// ip=<client>:<server>:<gw>:<mask>:<hostname>:<iface>:<autoconf> — server AND gw
	// must BOTH be empty (three consecutive colons before the netmask): no default
	// route in the guest is part of the isolation proof. Asserting the full prefix
	// through the netmask is what makes this test able to fail — a shorter substring
	// matches even when a gateway is present.
	want := "console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 root=/dev/vda ro init=/init " +
		"ip=172.30.0.2:::255.255.255.252::eth0:off"
	if i.BootArgs != want {
		t.Errorf("boot args:\n got: %s\nwant: %s", i.BootArgs, want)
	}
	if !strings.Contains(i.BootArgs, "console=ttyS0") {
		t.Errorf("need serial console for the proof log, got: %s", i.BootArgs)
	}
	// S-1: the ip= parameter configures IPv4 ONLY. With an IPv6 stack the guest gets a
	// link-local address regardless and can transmit Router Advertisements at a host
	// whose accept_ra defaults to 1 — i.e. become the host's IPv6 gateway. Killing the
	// stack at boot dissolves the class at the source ("isolation is physics").
	if !strings.Contains(i.BootArgs, "ipv6.disable=1") {
		t.Errorf("guest must boot with no IPv6 stack (S-1), got: %s", i.BootArgs)
	}
	// S-9: do not rely on Firecracker inferring the root parameter from is_root_device.
	if !strings.Contains(i.BootArgs, "root=/dev/vda") {
		t.Errorf("root device must be explicit (S-9), got: %s", i.BootArgs)
	}
}

func TestPlan_LeakDemoSetsGateway(t *testing.T) {
	t.Setenv("SKIFF_LEAK_DEMO", "1")
	i := Plan(cfg(), "/bundle", 0)
	// Deliberate-leak mode (design review B2): the guest gets the host as default gateway
	// so the escape path exists and the guard's failure direction can be demonstrated.
	want := "console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 root=/dev/vda ro init=/init " +
		"ip=172.30.0.2::172.30.0.1:255.255.255.252::eth0:off"
	if i.BootArgs != want {
		t.Errorf("leak-demo boot args:\n got: %s\nwant: %s", i.BootArgs, want)
	}
}

func TestPlan_RunDirDefaultsOffTheBundle(t *testing.T) {
	// S-4: sockets and console logs must NOT live in the bundle — the bundle runs from
	// a USB stick and vfat/exFAT cannot hold a unix domain socket at all.
	i := Plan(cfg(), "/bundle", 0)
	if i.Socket != "/tmp/skiff-run.d/skiff-0.sock" {
		t.Errorf("Socket = %q, want /tmp/skiff-run.d/skiff-0.sock", i.Socket)
	}
	if i.ConsoleLog != "/tmp/skiff-run.d/skiff-0.console.log" {
		t.Errorf("ConsoleLog = %q, want /tmp/skiff-run.d/skiff-0.console.log", i.ConsoleLog)
	}
	if strings.Contains(i.Socket, "/bundle") || strings.Contains(i.ConsoleLog, "/bundle") {
		t.Errorf("run dir must never be bundle-relative, got %q / %q", i.Socket, i.ConsoleLog)
	}
}

func TestPlan_RunDirHonorsEnvOverride(t *testing.T) {
	t.Setenv("SKIFF_RUN_DIR", "/run/user/1000/skiff")
	i := Plan(cfg(), "/bundle", 1)
	if i.Socket != "/run/user/1000/skiff/skiff-1.sock" {
		t.Errorf("Socket = %q, want /run/user/1000/skiff/skiff-1.sock", i.Socket)
	}
	if i.ConsoleLog != "/run/user/1000/skiff/skiff-1.console.log" {
		t.Errorf("ConsoleLog = %q, want /run/user/1000/skiff/skiff-1.console.log", i.ConsoleLog)
	}
}
