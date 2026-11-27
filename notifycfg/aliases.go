// Package notifycfg is what a notification configuration says and how it is
// judged: the alias map of /etc/notify.yaml, its targets, the rules a save must
// meet (it parses, it keeps the system alias, it refers only to secrets that
// exist), and the Service that applies them over notify.
//
// It does not send anything: a target kind is the alerting package's.
package notifycfg

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SystemAlias is the reserved alias name in notify.yaml that receives alerts
// the platform raises on a group's behalf, as opposed to alerts a cron's own
// `alert` stage raises. It is an ordinary alias with ordinary targets, except
// that it always exists: a build creates it when it sets a group up (the hosted
// build starts it as the owner's email) and Validate refuses a save without it.
// Today the only such alerts are a cron being disabled or benched.
const SystemAlias = "system"

// AliasMap is the parsed form of /etc/notify.yaml. Keys are alias names
// that cron watches reference via `notify: <alias>`. Each value bundles
// per-service target lists.
type AliasMap map[string]*Alias

// Alias is one entry in the alias map.
type Alias struct {
	Slack     []SlackTarget   `yaml:"slack,omitempty"`
	PagerDuty []PDTarget      `yaml:"pagerduty,omitempty"`
	Webhook   []WebhookTarget `yaml:"webhook,omitempty"`
	Email     []EmailTarget   `yaml:"email,omitempty"`

	// Extra holds every other key of the alias: the target kinds an adapter
	// adds. The store does not interpret them; the notifier hands each entry
	// to the AlertTarget registered for its kind.
	Extra map[string]yaml.Node `yaml:",inline"`
}

// AliasEntries is one kind's target entries of an alias, as YAML nodes.
type AliasEntries struct {
	Kind    string
	Entries []*yaml.Node
}

// Targets lists the alias's entries by kind: the built-in kinds in the order
// slack, pagerduty, webhook, email, then the others by name. A kind whose value
// is a sequence has one entry per element; any other value is a single entry.
func (a *Alias) Targets() []AliasEntries {
	var out []AliasEntries
	add := func(kind string, v any, n int) {
		if n == 0 {
			return
		}
		var seq yaml.Node
		if err := seq.Encode(v); err != nil {
			return
		}
		e := AliasEntries{Kind: kind}
		for _, c := range seq.Content {
			e.Entries = append(e.Entries, c)
		}
		out = append(out, e)
	}
	add("slack", a.Slack, len(a.Slack))
	add("pagerduty", a.PagerDuty, len(a.PagerDuty))
	add("webhook", a.Webhook, len(a.Webhook))
	add("email", a.Email, len(a.Email))
	kinds := make([]string, 0, len(a.Extra))
	for k := range a.Extra {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		n := a.Extra[k]
		if n.Kind == yaml.SequenceNode {
			out = append(out, AliasEntries{Kind: k, Entries: n.Content})
		} else {
			out = append(out, AliasEntries{Kind: k, Entries: []*yaml.Node{&n}})
		}
	}
	return out
}

// SlackTarget is a single Slack destination. Accepts either a bare string
// (the channel name, e.g. "#ops") or a mapping with `channel:` and an optional
// `token:` naming the secret that holds the credential, `${secrets.NAME}`; the
// default is the secret `slack_token`.
type SlackTarget struct {
	Channel string `yaml:"channel"`
	Token   string `yaml:"token,omitempty"`
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
			Token   string `yaml:"token"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.Channel == "" {
			return fmt.Errorf("slack target missing `channel`")
		}
		if err := checkTokenRef(raw.Token); err != nil {
			return fmt.Errorf("slack target: %w", err)
		}
		s.Channel, s.Token = raw.Channel, raw.Token
		return nil
	}
	return fmt.Errorf("slack target must be a string or mapping")
}

// MarshalYAML emits a bare string when no options are set so round-trips
// preserve the ergonomic form.
func (s SlackTarget) MarshalYAML() (any, error) {
	if s.Token == "" {
		return s.Channel, nil
	}
	return map[string]string{"channel": s.Channel, "token": s.Token}, nil
}

// checkTokenRef accepts an empty token (the default secret) or exactly one
// ${secrets.NAME} reference.
func checkTokenRef(token string) error {
	if token == "" {
		return nil
	}
	if m := secretRefRe.FindStringSubmatch(token); m == nil || m[0] != token {
		return fmt.Errorf("`token:` must be a single ${secrets.NAME} reference, got %q", token)
	}
	return nil
}

// TokenSecret is the name of the secret that holds the credential: the one the
// entry's `token:` references, else def.
func TokenSecret(token, def string) string {
	if m := secretRefRe.FindStringSubmatch(token); m != nil {
		return m[1]
	}
	return def
}

// PDTarget is a single PagerDuty destination. Accepts either a bare string
// (the integration name) or a mapping with `name:` and optional `severity:` and
// `token:`. The routing key is the secret `pagerduty_<name>`, or the one `token:`
// references, `${secrets.NAME}`.
type PDTarget struct {
	Name     string `yaml:"name"`
	Severity string `yaml:"severity,omitempty"`
	Token    string `yaml:"token,omitempty"`
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
			Token    string `yaml:"token"`
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
		if err := checkTokenRef(raw.Token); err != nil {
			return fmt.Errorf("pagerduty target: %w", err)
		}
		p.Name, p.Severity, p.Token = raw.Name, raw.Severity, raw.Token
		return nil
	}
	return fmt.Errorf("pagerduty target must be a string or mapping")
}

// WebhookTarget is a single webhook destination. Accepts either a bare string
// (the URL) or a mapping with `url:` and optional `headers:`.
type WebhookTarget struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// UnmarshalYAML lets a WebhookTarget be written as either a plain string or
// a mapping with `url:` / `headers:`.
func (w *WebhookTarget) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		w.URL = node.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			URL     string            `yaml:"url"`
			Headers map[string]string `yaml:"headers"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.URL == "" {
			return fmt.Errorf("webhook target missing `url`")
		}
		w.URL = raw.URL
		w.Headers = raw.Headers
		return nil
	}
	return fmt.Errorf("webhook target must be a string or mapping")
}

// MarshalYAML emits a bare string when no headers are set.
func (w WebhookTarget) MarshalYAML() (any, error) {
	if len(w.Headers) == 0 {
		return w.URL, nil
	}
	type plain WebhookTarget
	return plain(w), nil
}

// EmailTarget is a single email destination. Accepts a bare string (the
// address) or a mapping with `address:`.
type EmailTarget struct {
	Address string `yaml:"address"`
}

// UnmarshalYAML lets an EmailTarget be written as a plain string or a mapping.
func (e *EmailTarget) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		e.Address = node.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			Address string `yaml:"address"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.Address == "" {
			return fmt.Errorf("email target missing `address`")
		}
		e.Address = raw.Address
		return nil
	}
	return fmt.Errorf("email target must be a string or mapping")
}

// MarshalYAML emits a bare string so round-trips preserve the ergonomic form.
func (e EmailTarget) MarshalYAML() (any, error) {
	return e.Address, nil
}

// Parse parses a /etc/notify.yaml document. Empty input parses
// to an empty map without error (customers without any aliases defined).
func Parse(raw string) (AliasMap, error) {
	if strings.TrimSpace(raw) == "" {
		return AliasMap{}, nil
	}
	var m AliasMap
	if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("notify.yaml: %w", err)
	}
	for name, alias := range m {
		if alias == nil {
			return nil, fmt.Errorf("notify.yaml: alias %q is empty", name)
		}
		if len(alias.Slack) == 0 && len(alias.PagerDuty) == 0 &&
			len(alias.Webhook) == 0 && len(alias.Email) == 0 && len(alias.Extra) == 0 {
			return nil, fmt.Errorf("notify.yaml: alias %q has no targets", name)
		}
		for i, t := range alias.PagerDuty {
			if t.Severity == "" {
				alias.PagerDuty[i].Severity = "error"
			}
		}
	}
	return m, nil
}

// Validate is the check every write of notify.yaml goes through:
// the document must parse, and it must keep a SystemAlias with at least one
// target. The alias can be repointed freely but not removed, because it is the
// one destination weft can count on for alerts it raises on the group's behalf.
func Validate(raw string) error {
	m, err := Parse(raw)
	if err != nil {
		return err
	}
	if m[SystemAlias] == nil {
		return fmt.Errorf("notify.yaml: the %q alias is required: it receives the alerts weft raises on your behalf. Change its targets, but don't remove it", SystemAlias)
	}
	return nil
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
