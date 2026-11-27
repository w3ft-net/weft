package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// A Source supplies the runtime settings as a flat map of UPPER_SNAKE keys to
// values (see Overlay for the keys that mean something). An error means the
// source could not be read; a source with nothing set returns an empty map.
type Source interface {
	Load(ctx context.Context) (map[string]string, error)
}

// FileSource reads a YAML file of flat key: value pairs. A missing file is an
// empty source, so a deployment that sets nothing needs no file.
type FileSource struct{ Path string }

func (f FileSource) Load(context.Context) (map[string]string, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out, nil
}

// EnvSource is the process environment: a key set there is a setting, so any
// file value can be overridden per process.
type EnvSource struct{}

func (EnvSource) Load(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// Layered merges sources, a later one overriding an earlier one's keys. It fails
// when any layer does.
type Layered []Source

func (l Layered) Load(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range l {
		m, err := s.Load(ctx)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// DefaultFile is where the file source looks when WEFT_CONFIG_FILE is unset.
const DefaultFile = "/etc/weft/config.yaml"

// Kind is a source a deployment can offer by name, chosen with
// WEFT_CONFIG_SOURCE. The file source is always there; an extension supplies the
// others (ext/config/ssm).
type Kind struct {
	Name string
	// New builds the source. region is where the deployment's cloud is, and may be
	// empty to use the SDK's own resolution.
	New func(region string) Source
}

// SourceFromEnv is the source a process runs on when its deployment names none:
// the YAML file at WEFT_CONFIG_FILE (else DefaultFile), overridden by the
// environment. WEFT_CONFIG_SOURCE=<name> reads the named kind instead of the file
// (still overridden by the environment); a name no kind answers to is a mistake
// and is reported by the returned source on every load, so it is never silently
// read as no configuration.
func SourceFromEnv(region string, kinds ...Kind) Source {
	var base Source
	switch name := os.Getenv("WEFT_CONFIG_SOURCE"); name {
	case "", "file":
		path := os.Getenv("WEFT_CONFIG_FILE")
		if path == "" {
			path = DefaultFile
		}
		base = FileSource{Path: path}
	default:
		for _, k := range kinds {
			if k.Name == name {
				base = k.New(region)
			}
		}
		if base == nil {
			base = unknownKind(name)
		}
	}
	return Layered{base, EnvSource{}}
}

type unknownKind string

func (u unknownKind) Load(context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("WEFT_CONFIG_SOURCE=%q: this build has no such config source", string(u))
}
