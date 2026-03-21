package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Resources.VCPUs != DefaultVCPUs {
		t.Errorf("expected VCPUs %d, got %d", DefaultVCPUs, cfg.Resources.VCPUs)
	}
	if cfg.Resources.MemoryMiB != DefaultMemoryMiB {
		t.Errorf("expected MemoryMiB %d, got %d", DefaultMemoryMiB, cfg.Resources.MemoryMiB)
	}
	if cfg.Debug.Enabled {
		t.Error("expected debug disabled by default")
	}
}

func TestLoadNonExistent(t *testing.T) {
	cfg, err := Load("/nonexistent/config.toml")
	if err != nil {
		t.Fatalf("expected no error for nonexistent file, got %v", err)
	}
	if cfg.Resources.VCPUs != DefaultVCPUs {
		t.Errorf("expected default VCPUs")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
[resources]
vcpus = 4
memory_mib = 1024

[debug]
enabled = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Resources.VCPUs != 4 {
		t.Errorf("expected vcpus 4, got %d", cfg.Resources.VCPUs)
	}
	if cfg.Resources.MemoryMiB != 1024 {
		t.Errorf("expected memory_mib 1024, got %d", cfg.Resources.MemoryMiB)
	}
	if !cfg.Debug.Enabled {
		t.Error("expected debug.enabled = true")
	}
}
