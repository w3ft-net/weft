package shell

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the .weft.yaml configuration.
type Config struct {
	Endpoint string `yaml:"endpoint"` // coordinator gRPC address (host:port)
	Token    string `yaml:"token"`    // auth token (future use)
	Customer string `yaml:"customer"` // customer ID
}

// LoadConfig reads .weft.yaml from the current directory or home directory.
func LoadConfig() (*Config, error) {
	// Try current directory first.
	if cfg, err := loadConfigFrom(".weft.yaml"); err == nil {
		return cfg, nil
	}

	// Try home directory.
	home, err := os.UserHomeDir()
	if err == nil {
		if cfg, err := loadConfigFrom(filepath.Join(home, ".weft.yaml")); err == nil {
			return cfg, nil
		}
	}

	return nil, fmt.Errorf("no .weft.yaml found in current directory or home directory")
}

func loadConfigFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
