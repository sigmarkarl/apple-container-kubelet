package config

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

const (
	DefaultConfigPath = "/etc/apple-container-kubelet/config.toml"
	DefaultVCPUs      = 1
	DefaultMemoryMiB  = 1024
)

type Config struct {
	Resources ResourceConfig `toml:"resources"`
	TLS       TLSConfig      `toml:"tls"`
	Debug     DebugConfig    `toml:"debug"`
}

type TLSConfig struct {
	ClientCAPath string `toml:"client_ca_path"`
}

type ResourceConfig struct {
	VCPUs     uint   `toml:"vcpus"`
	MemoryMiB uint64 `toml:"memory_mib"`
}

type DebugConfig struct {
	Enabled bool `toml:"enabled"`
}

func DefaultConfig() *Config {
	retVal := &Config{
		Resources: ResourceConfig{
			VCPUs:     DefaultVCPUs,
			MemoryMiB: DefaultMemoryMiB,
		},
	}
	return retVal
}

// Load reads configuration from the given path, falling back to defaults.
func Load(path string) (*Config, error) {
	retVal := DefaultConfig()

	if path == "" {
		path = DefaultConfigPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return retVal, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, retVal); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return retVal, nil
}
