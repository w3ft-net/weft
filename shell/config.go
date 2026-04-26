package shell

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TokenClaims holds the JWT fields the CLI needs for prompt rendering
// and `weft whoami`. Decoded WITHOUT verifying the signature — the CLI
// doesn't have the issuing secret — so callers must not trust these
// values for security decisions; it's display-only.
type TokenClaims struct {
	Email    string `json:"email"`
	Customer string `json:"customer"`
	Tier     string `json:"tier"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// DecodeClaims parses the JWT payload segment without checking the
// signature. Used to surface the logged-in identity in the shell
// prompt and `weft whoami`.
func DecodeClaims(token string) (*TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims TokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	return &claims, nil
}

// EmailLocalPart returns the part of an email before the '@', or the
// whole string if no '@' is present. Used as the user portion of the
// shell prompt — alice@acme.test → "alice".
func EmailLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// Config is the .weft.yaml configuration. Endpoint must be an http(s)
// URL pointing at the weft api server; the CLI speaks HTTP end-to-end
// now (no direct-to-coordinator gRPC). `weft login` writes this file
// automatically after the device-flow round-trip.
type Config struct {
	Endpoint string `yaml:"endpoint"` // api base URL, e.g. https://api.w3ft.net
	Token    string `yaml:"token"`    // JWT from the device flow
}

// LoadConfig resolves configuration from the following chain (first wins):
//  1. Environment variables: WEFT_ENDPOINT, WEFT_TOKEN
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
	if v := os.Getenv("WEFT_TOKEN"); v != "" {
		cfg.Token = v
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("no endpoint configured — run `weft login` or set WEFT_ENDPOINT")
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
