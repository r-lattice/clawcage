package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Instances int    `yaml:"instances"`
	RAMMiB    int    `yaml:"ram_mib"`
	VCPUs     int    `yaml:"vcpus"`
	Model     string `yaml:"model"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	var problems []error
	if c.Instances < 1 || c.Instances > 8 {
		problems = append(problems, fmt.Errorf("instances must be 1..8, got %d", c.Instances))
	}
	if c.RAMMiB < 2048 {
		problems = append(problems, fmt.Errorf("ram_mib must be >= 2048, got %d", c.RAMMiB))
	}
	if c.VCPUs < 1 {
		problems = append(problems, fmt.Errorf("vcpus must be >= 1, got %d", c.VCPUs))
	}
	if c.Model == "" {
		problems = append(problems, errors.New("model must be set (filename under models/)"))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("config %s invalid:\n%w", path, errors.Join(problems...))
	}
	return &c, nil
}
