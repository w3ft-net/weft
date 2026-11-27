// Package alerting is the contract between the alert dispatcher and the kinds
// of destination an alert can reach: the alias key of /etc/notify.yaml
// (`webhook:`, `email:`, ...), what an entry of that kind means, and how it is
// sent. It holds the core's own kinds, webhook and email. A deployment adds
// others by registering an AlertTarget. The package is cgo-free so the api and
// the filesystem mounts can validate a save against the kinds the build sends.
package alerting

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
)

// Timeout is the default per-target delivery budget. Targets that make HTTP
// calls give their client the same.
const Timeout = 10 * time.Second

// Alert is one notification handed to a target.
type Alert struct {
	Owner shape.Added
	Alias string // the notify.yaml alias being dispatched
	Cron  string // the cron (or synthetic ad-hoc name) that fired; stable across a repeating incident
	Body  string // the alert text; a target truncates it to its service's limits
}

// Destination is one configured entry of a target kind, ready to send to.
type Destination struct {
	// Key identifies the destination within its alias. It is recorded when
	// the destination has been reached, so a retry of the same incident skips
	// it. It must not carry a secret.
	Key string
	// Name is what the logs call the destination.
	Name string
	// Send delivers the alert. It runs under the dispatcher's per-target budget.
	Send func(ctx context.Context, a Alert) error
}

// AlertTarget is a kind of alert destination: the key under which its entries
// appear in an alias of /etc/notify.yaml.
type AlertTarget interface {
	// Kind is the alias key the target handles.
	Kind() string
	// Open reads one entry of the alias's list for Kind, a bare scalar or a
	// mapping, into a destination. It only validates and builds: nothing is
	// sent and no credential is read, so a registry can open entries in a
	// process that holds none. An error means the entry is invalid.
	Open(entry *yaml.Node) (Destination, error)
}

// Registry is the set of kinds a build sends.
type Registry struct {
	targets map[string]AlertTarget
}

// SecretResolver reads the secrets targets deliver with: it fills in the
// ${secrets.NAME} references of a webhook target and returns a named secret's
// value for the others; notifycfg.Service is one.
type SecretResolver interface {
	Secret(owner shape.Added, name string) (string, error)
	ResolveWebhook(owner shape.Added, t notifycfg.WebhookTarget) (notifycfg.WebhookTarget, *strings.Replacer, error)
}

// NewRegistry is the core's kinds over secrets (webhook, slack, pagerduty, and
// email over the SMTP the environment names) plus the given targets. A given
// "slack", "pagerduty" or "email" target replaces the core's, for a deployment
// with credentials or SMTP settings of its own. Any other kind given twice is a
// wiring mistake and panics.
func NewRegistry(secrets SecretResolver, targets ...AlertTarget) *Registry {
	r := &Registry{}
	r.Register(NewWebhookTarget(secrets))
	for _, t := range targets {
		r.Register(t)
	}
	for _, d := range []AlertTarget{NewSlackTarget(secrets), NewPagerDutyTarget(secrets), EmailTarget{SMTP: SMTPFromEnv()}} {
		if _, ok := r.targets[d.Kind()]; !ok {
			r.Register(d)
		}
	}
	return r
}

// Register adds a kind. A kind registered twice panics.
func (r *Registry) Register(t AlertTarget) {
	if t == nil {
		return
	}
	if _, dup := r.targets[t.Kind()]; dup {
		panic(fmt.Sprintf("alerting: alert target kind %q registered twice", t.Kind()))
	}
	if r.targets == nil {
		r.targets = map[string]AlertTarget{}
	}
	r.targets[t.Kind()] = t
}

// Lookup is the target for kind.
func (r *Registry) Lookup(kind string) (AlertTarget, bool) {
	t, ok := r.targets[kind]
	return t, ok
}

// Validate reports the first entry of aliases that a registered kind rejects,
// and the first kind that has no registered target. Nothing is sent.
func (r *Registry) Validate(aliases notifycfg.AliasMap) error {
	for _, name := range aliases.AliasNames() {
		for _, k := range aliases[name].Targets() {
			t, ok := r.targets[k.Kind]
			if !ok {
				return fmt.Errorf("alias %q: no %q alert target in this build", name, k.Kind)
			}
			for _, e := range k.Entries {
				if _, err := t.Open(e); err != nil {
					return fmt.Errorf("alias %q: %s: %w", name, k.Kind, err)
				}
			}
		}
	}
	return nil
}

// Truncate cuts s to n bytes and says so, for a target whose service limits a
// message's size.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}
