package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	c, err := Load(write(t, "instances: 2\nram_mib: 6144\nvcpus: 4\nmodel: qwen3-8b-q4.gguf\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Instances != 2 || c.RAMMiB != 6144 || c.VCPUs != 4 || c.Model != "qwen3-8b-q4.gguf" {
		t.Fatalf("bad parse: %+v", c)
	}
}

func TestLoad_MissingFileIsLoud(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad_CollectsALLProblems(t *testing.T) {
	_, err := Load(write(t, "instances: 0\nram_mib: 512\nvcpus: 0\nmodel: \"\"\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"instances", "ram_mib", "vcpus", "model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}
