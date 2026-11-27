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

// SourceFromEnv is the source a process runs on when its deployment names none:
// the YAML file at WEFT_CONFIG_FILE (else DefaultFile), overridden by the
// environment. WEFT_CONFIG_SOURCE=ssm reads AWS SSM instead (APP_NAME, default
// "weft", names the parameter), still overridden by the environment; region is
// where SSM is, and may be empty to use the SDK's own resolution.
func SourceFromEnv(region string) Source {
	var base Source
	switch os.Getenv("WEFT_CONFIG_SOURCE") {
	case "ssm":
		app := os.Getenv("APP_NAME")
		if app == "" {
			app = "weft"
		}
		base = SSMSource{AppName: app, Region: region}
	default:
		path := os.Getenv("WEFT_CONFIG_FILE")
		if path == "" {
			path = DefaultFile
		}
		base = FileSource{Path: path}
	}
	return Layered{base, EnvSource{}}
}
