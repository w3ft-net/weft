// Package cluster is the facts the weft servers share, written once. The api,
// the coordinator, compute and the shards each dial or are dialed by another:
// where the coordinator listens is both a listen flag and a dial flag, and they
// have to agree. A cluster file says it once, and each server takes from it
// the flags it would otherwise have been given.
//
// The file is a default. A flag given on the command line wins, so every flag
// keeps working as it did, and a server with no file behaves as before. The file
// says nothing of how a deployment stores its data: that is the deployment's
// own configuration.
package cluster

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// EnvPath is the environment variable that names the cluster file when --config
// is not given.
const EnvPath = "WEFT_CONFIG"

// The servers a Config speaks to.
const (
	API         = "api"
	Coordinator = "coordinator"
	Compute     = "compute"
	Shard       = "shard"
)

// Config is the cluster file. Every field is optional: one left out leaves the
// server's own default.
type Config struct {
	// Region is the AWS region every server runs in.
	Region string `yaml:"region"`
	// BaseURL is the origin the api builds magic links on and the coordinator
	// builds share links on.
	BaseURL string `yaml:"base_url"`
	// APIBaseURL is the api's own external origin.
	APIBaseURL string `yaml:"api_base_url"`
	Redis   struct {
		Addr string `yaml:"addr"`
		TLS  bool   `yaml:"tls"`
	} `yaml:"redis"`
	Coordinator struct {
		// Host is where the shards reach the coordinator.
		Host string `yaml:"host"`
		// LocalPort is the loopback port the coordinator serves its full service
		// on and the co-located api dials.
		LocalPort int `yaml:"local_port"`
		// InternalPort is the port the shards report their heartbeats to.
		InternalPort int `yaml:"internal_port"`
	} `yaml:"coordinator"`
	Compute struct {
		// Host is where the coordinator reaches compute.
		Host string `yaml:"host"`
		// Port is the port compute listens on and the coordinator dials.
		Port int `yaml:"port"`
	} `yaml:"compute"`
}

// Load reads the cluster file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Flags are the flag defaults the file gives the named server, by flag name.
func (c *Config) Flags(server string) map[string]string {
	out := map[string]string{}
	set := func(name, v string) {
		if v != "" {
			out[name] = v
		}
	}
	port := func(p int) string {
		if p == 0 {
			return ""
		}
		return strconv.Itoa(p)
	}
	local, internal, compute := port(c.Coordinator.LocalPort), port(c.Coordinator.InternalPort), port(c.Compute.Port)
	redis := func() {
		set("redis-addr", c.Redis.Addr)
		if c.Redis.TLS {
			set("redis-tls", "true")
		}
	}
	switch server {
	case API:
		if local != "" {
			set("coordinator", "localhost:"+local)
		}
		set("base-url", c.BaseURL)
		set("api-base-url", c.APIBaseURL)
		redis()
	case Coordinator:
		if local != "" {
			set("local-addr", "127.0.0.1:"+local)
		}
		if internal != "" {
			set("internal-addr", ":"+internal)
		}
		if c.Compute.Host != "" && compute != "" {
			set("compute", c.Compute.Host+":"+compute)
		}
		set("region", c.Region)
		set("share-url-base", c.BaseURL)
		redis()
	case Compute:
		set("port", compute)
	case Shard:
		if c.Coordinator.Host != "" && internal != "" {
			set("coordinator-addr", c.Coordinator.Host+":"+internal)
		}
	}
	return out
}

// Env is the environment defaults the file gives the named server: the
// variables a server reads that no flag carries.
func (c *Config) Env(server string) map[string]string {
	out := map[string]string{}
	if c.Region != "" && (server == API || server == Shard || server == Compute) {
		out["AWS_REGION"] = c.Region
	}
	return out
}

// DefineFlag registers --config on fs and returns where its value goes. It is
// defined apart from a server's own flags, which are pinned by their golden
// files.
func DefineFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "cluster file of the settings the weft servers share ($"+EnvPath+" when empty); a flag given here wins over it")
}

// Overlay applies the cluster file at path, or at $WEFT_CONFIG when path is
// empty, to the server's flags and environment. A flag the command line set,
// and a variable already in the environment, are left alone. With no file it
// does nothing. Call it after fs is parsed.
func Overlay(fs *flag.FlagSet, server, path string) error {
	if path == "" {
		path = os.Getenv(EnvPath)
	}
	if path == "" {
		return nil
	}
	c, err := Load(path)
	if err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for name, v := range c.Flags(server) {
		if given[name] || fs.Lookup(name) == nil {
			continue
		}
		if err := fs.Set(name, v); err != nil {
			return fmt.Errorf("%s: %s: %w", path, name, err)
		}
	}
	for k, v := range c.Env(server) {
		if _, ok := os.LookupEnv(k); !ok {
			os.Setenv(k, v)
		}
	}
	return nil
}
