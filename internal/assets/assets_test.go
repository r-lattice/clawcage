package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeBundle(t *testing.T, omit string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]os.FileMode{
		"bin/firecracker":    0o755,
		"kernel/vmlinux":     0o644,
		"rootfs/rootfs.ext4": 0o644,
		"models/q.gguf":      0o644,
		"models.ext4":        0o644,
	}
	for rel, mode := range files {
		if rel == omit {
			continue
		}
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestValidate_CompleteBundlePasses(t *testing.T) {
	if err := Validate(makeBundle(t, ""), "q.gguf"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_NamesEveryMissingAsset(t *testing.T) {
	root := makeBundle(t, "kernel/vmlinux")
	if err := os.Remove(filepath.Join(root, "rootfs/rootfs.ext4")); err != nil {
		t.Fatal(err)
	}
	err := Validate(root, "q.gguf")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"kernel/vmlinux", "rootfs/rootfs.ext4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}

func TestValidate_FirecrackerMustBeExecutable(t *testing.T) {
	root := makeBundle(t, "")
	if err := os.Chmod(filepath.Join(root, "bin/firecracker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root, "q.gguf"); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("want not-executable error, got: %v", err)
	}
}
