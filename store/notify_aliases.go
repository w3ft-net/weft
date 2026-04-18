package store

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AliasMap is the parsed form of /etc/notify.conf. Keys are alias names
// that cron watches reference via `notify: <alias>`. Each value bundles
// per-service target lists.
type AliasMap map[string]*Alias

// Alias is one entry in the alias map.
type Alias struct {
	Slack     []SlackTarget `yaml:"slack,omitempty"`
	PagerDuty []PDTarget    `yaml:"pagerduty,omitempty"`
}

// SlackTarget is a single Slack destination. Accepts either a bare string
// (the channel name, e.g. "#ops") or a YAML mapping for future options.
type SlackTarget struct {
	Channel string `yaml:"channel"`
}

// UnmarshalYAML lets a SlackTarget be written as either a plain string or
// a mapping with a `channel:` key.
func (s *SlackTarget) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s.Channel = node.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			Channel string `yaml:"channel"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.Channel == "" {
			return fmt.Errorf("slack target missing `channel`")
		}
		s.Channel = raw.Channel
		return nil
	}
	return fmt.Errorf("slack target must be a string or mapping")
}

// MarshalYAML emits a bare string when no options are set so round-trips
// preserve the ergonomic form.
func (s SlackTarget) MarshalYAML() (any, error) {
	return s.Channel, nil
}

// PDTarget is a single PagerDuty destination. Accepts either a bare string
// (the integration name) or a mapping with `name:` and optional `severity:`.
type PDTarget struct {
	Name     string `yaml:"name"`
	Severity string `yaml:"severity,omitempty"`
}

// ValidPDSeverities enumerates the PagerDuty Events v2 severity levels.
var ValidPDSeverities = map[string]bool{
	"critical": true,
	"error":    true,
	"warning":  true,
	"info":     true,
}

// UnmarshalYAML lets a PDTarget be written as either a plain string or a
// mapping with `name:` / `severity:`.
func (p *PDTarget) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Name = node.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			Name     string `yaml:"name"`
			Severity string `yaml:"severity"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.Name == "" {
			return fmt.Errorf("pagerduty target missing `name`")
		}
		if raw.Severity != "" && !ValidPDSeverities[raw.Severity] {
			return fmt.Errorf("pagerduty severity %q not one of critical/error/warning/info", raw.Severity)
		}
		p.Name = raw.Name
		p.Severity = raw.Severity
		return nil
	}
	return fmt.Errorf("pagerduty target must be a string or mapping")
}

// ParseNotifyAliases parses a /etc/notify.conf document. Empty input parses
// to an empty map without error (customers without any aliases defined).
func ParseNotifyAliases(raw string) (AliasMap, error) {
	if strings.TrimSpace(raw) == "" {
		return AliasMap{}, nil
	}
	var m AliasMap
	if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("notify.conf: %w", err)
	}
	for name, alias := range m {
		if alias == nil {
			return nil, fmt.Errorf("notify.conf: alias %q is empty", name)
		}
		if len(alias.Slack) == 0 && len(alias.PagerDuty) == 0 {
			return nil, fmt.Errorf("notify.conf: alias %q has no targets", name)
		}
		for i, t := range alias.PagerDuty {
			if t.Severity == "" {
				alias.PagerDuty[i].Severity = "error"
			}
		}
	}
	return m, nil
}

// AliasNames returns the alias keys in sorted order (handy for UI display).
func (m AliasMap) AliasNames() []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
