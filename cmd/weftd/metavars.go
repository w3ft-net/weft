package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/graybeardcorp/weft/internal/weftlog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/weftd"
	"gopkg.in/yaml.v3"
)

// MetavariableConfig says where one metavariable's value comes from. Exactly
// one field is set.
type MetavariableConfig struct {
	Env  string `yaml:"env,omitempty"`  // environment-variable name; read once at startup
	File string `yaml:"file,omitempty"` // file whose trimmed contents are the value; re-read on change
	JSON string `yaml:"json,omitempty"` // <path>:.<json.path>; the extracted field is the value; re-read on change
}

// allowedMetavariables lists the columns the `metavariables:` map can
// populate. `env`, `app`, `instance`, `level`, `ts` and the k8s-derived
// columns are bound elsewhere (token, path, schema) and cannot be overridden.
var allowedMetavariables = map[string]bool{
	"deployment": true,
	"version":    true,
	"region":     true,
	"cluster":    true,
	"node":       true,
}

// metavarSet holds the live reader for each configured column.
type metavarSet map[string]*weftd.ValueReader

// metavariables is the host-wide set from the top-level `metavariables:` key.
// Built once at startup and read lock-free from the row-stamping path.
var metavariables metavarSet

// sourceMetavariables holds each source's own set, keyed by fullApp. A glob
// source registers one entry per matched file, at runtime, so this is a
// sync.Map; all of a glob's files share the same readers.
var sourceMetavariables sync.Map // fullApp → metavarSet

// newMetavariable builds the reader for one configured metavariable and
// returns a short description of its source for the startup log.
func newMetavariable(ctx context.Context, mc MetavariableConfig) (*weftd.ValueReader, string, error) {
	var set []string
	for name, v := range map[string]string{"env": mc.Env, "file": mc.File, "json": mc.JSON} {
		if v != "" {
			set = append(set, name)
		}
	}
	if len(set) != 1 {
		return nil, "", fmt.Errorf("set exactly one of env:, file: or json: (got %d)", len(set))
	}
	switch {
	case mc.Env != "":
		return weftd.NewStaticValueReader(os.Getenv(mc.Env)), "env " + mc.Env, nil
	case mc.File != "":
		return weftd.NewValueReader(ctx, weftd.ValueSpec{Path: mc.File}), "file " + mc.File, nil
	default:
		spec, err := weftd.ParseJSONSpec(mc.JSON)
		if err != nil {
			return nil, "", err
		}
		return weftd.NewValueReader(ctx, spec), "json " + mc.JSON, nil
	}
}

// buildMetavariables builds a readers set from one `metavariables:` map. where
// names the map in errors and the startup log ("metavariables", or
// "sources[2].metavariables"). Bad config is an error rather than a silently
// dropped stamp.
func buildMetavariables(ctx context.Context, where string, cfg map[string]MetavariableConfig) (metavarSet, error) {
	set := metavarSet{}
	for col, mc := range cfg {
		if !allowedMetavariables[col] {
			return nil, fmt.Errorf("%s: %q cannot be set here; allowed: deployment, version, region, cluster, node", where, col)
		}
		r, desc, err := newMetavariable(ctx, mc)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", where, col, err)
		}
		set[col] = r
		weftlog.Infof(ctx, "%s.%s: from %s", where, col, desc)
		if mc.Env != "" && r.Current() == "" {
			weftlog.Infof(ctx, "%s.%s: environment variable %s is unset", where, col, mc.Env)
		}
	}
	return set, nil
}

// initMetavariables builds the host-wide set and each source's own set from
// cfg. It returns the per-source sets by source index so the caller can
// register them under the fullApp names it derives.
func initMetavariables(ctx context.Context, cfg Config) (map[int]metavarSet, error) {
	var err error
	if metavariables, err = buildMetavariables(ctx, "metavariables", cfg.Metavariables); err != nil {
		return nil, err
	}
	perSource := map[int]metavarSet{}
	for i, src := range cfg.Sources {
		if len(src.Metavariables) == 0 {
			continue
		}
		if perSource[i], err = buildMetavariables(ctx, fmt.Sprintf("sources[%d].metavariables", i), src.Metavariables); err != nil {
			return nil, err
		}
	}
	return perSource, nil
}

// registerSource records one tailed stream's own metavariables, if it has
// any, under fullApp.
func registerSource(fullApp string, set metavarSet) {
	if len(set) > 0 {
		sourceMetavariables.Store(fullApp, set)
	}
}

// metavarValue returns a column's value for the source whose fullApp is app:
// the source's own value when it has one, else the host-wide value, else "".
// An empty app (metrics and other rows with no source) gets the host-wide value.
func metavarValue(app, col string) string {
	if set, ok := sourceMetavariables.Load(app); ok {
		if r := set.(metavarSet)[col]; r != nil {
			if v := r.Current(); v != "" {
				return v
			}
		}
	}
	if r := metavariables[col]; r != nil {
		return r.Current()
	}
	return ""
}

// stampMetavariables overlays the configured metavariables onto a LogRow from
// the source with fullApp app ("" for rows that have none). Operator config
// wins over auto-derived values (a `region` metavariable overrides the
// cloud-metadata probe). A column with no value leaves the row's own alone.
func stampMetavariables(row *pb.LogRow, app string) {
	if v := metavarValue(app, "deployment"); v != "" {
		row.Deployment = v
	}
	if v := metavarValue(app, "region"); v != "" {
		row.Region = v
	}
	if v := metavarValue(app, "version"); v != "" {
		row.Version = v
	}
	if v := metavarValue(app, "cluster"); v != "" {
		row.Cluster = v
	}
	if v := metavarValue(app, "node"); v != "" {
		row.Node = v
	}
}

// retiredConfigKeys names the config keys that earlier weftd versions read
// and this one does not: top-level `dimensions:` (now `metavariables:`) and
// the per-source `version:` block (now `metavariables:` on the source). The YAML
// decode is not strict, so without this a host that auto-updates onto this
// build would quietly stop stamping `node` and `version`.
func retiredConfigKeys(data []byte) []string {
	var old struct {
		Dimensions any `yaml:"dimensions"`
		Checkpoint struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"checkpoint"`
		Metrics struct {
			Enabled  *bool    `yaml:"enabled"`
			Services []string `yaml:"services"`
		} `yaml:"metrics"`
		Sources []struct {
			Version any `yaml:"version"`
		} `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &old); err != nil {
		return nil // the real parse reports it
	}
	var found []string
	if old.Checkpoint.Enabled != nil {
		found = append(found, "checkpoint.enabled: is gone; checkpointing is always on")
	}
	if old.Metrics.Enabled != nil || len(old.Metrics.Services) > 0 {
		found = append(found, "metrics.enabled: and metrics.services: are gone; the metrics: block itself turns metrics on (delete it to turn them off), and the rows go under your deployment")
	}
	if old.Dimensions != nil {
		found = append(found, "dimensions: is now metavariables: (each entry is env:, file: or json:)")
	}
	for _, s := range old.Sources {
		if s.Version != nil {
			found = append(found, "a source's version: is now metavariables: version: under that source")
			break
		}
	}
	sort.Strings(found)
	return found
}
