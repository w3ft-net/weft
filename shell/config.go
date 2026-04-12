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

// LoadConfig resolves configuration from the following chain (first wins):
//  1. Environment variables: WEFT_ENDPOINT, WEFT_CUSTOMER, WEFT_TOKEN
//  2. .weft.yaml in the current working directory
//  3. .weft.yaml in the user's home directory
//
// Env vars override individual fields — the file is still loaded for
// any fields not set in the environment.
func LoadConfig() (*Config, error) {
	// Load the file-based config (CWD then home).
	cfg := &Config{}
	if fileCfg, err := loadConfigFile(); err == nil {
		cfg = fileCfg
	}

	// Env var overrides.
	if v := os.Getenv("WEFT_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("WEFT_CUSTOMER"); v != "" {
		cfg.Customer = v
	}
	if v := os.Getenv("WEFT_TOKEN"); v != "" {
		cfg.Token = v
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("no endpoint configured — set WEFT_ENDPOINT or create .weft.yaml")
	}

	return cfg, nil
}

// loadConfigFile tries .weft.yaml from CWD then home directory.
func loadConfigFile() (*Config, error) {
	if cfg, err := loadConfigFrom(".weft.yaml"); err == nil {
		return cfg, nil
	}

	home, err := os.UserHomeDir()
	if err == nil {
		if cfg, err := loadConfigFrom(filepath.Join(home, ".weft.yaml")); err == nil {
			return cfg, nil
		}
	}

	return nil, fmt.Errorf("no .weft.yaml found")
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
