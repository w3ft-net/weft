package alerting

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/store"
)

// pigeon is a kind the core has never heard of, added through the registry.
type pigeon struct{}

func (pigeon) Kind() string { return "carrier-pigeon" }

func (pigeon) Open(entry *yaml.Node) (Destination, error) {
	var t struct {
		Coop string `yaml:"coop"`
	}
	if entry.Kind == yaml.ScalarNode {
		t.Coop = entry.Value
	} else if err := entry.Decode(&t); err != nil {
		return Destination{}, err
	}
	if t.Coop == "" {
		return Destination{}, errors.New("carrier-pigeon target missing `coop`")
	}
	return Destination{Key: "carrier-pigeon:" + t.Coop, Name: t.Coop, Send: func(context.Context, Alert) error { return nil }}, nil
}

func TestRegistry_ValidateOpensEveryEntry(t *testing.T) {
	r := NewRegistry(nil, pigeon{})
	ok, _ := store.ParseNotifyAliases("a:\n  carrier-pigeon: [north]\n  webhook: ['https://x.test']\n")
	if err := r.Validate(ok); err != nil {
		t.Errorf("valid aliases: %v", err)
	}
	bad, _ := store.ParseNotifyAliases("a:\n  carrier-pigeon:\n    - {nest: x}\n")
	if err := r.Validate(bad); err == nil || !strings.Contains(err.Error(), "coop") {
		t.Errorf("an entry the target rejects: err = %v", err)
	}
	unknown, _ := store.ParseNotifyAliases("a:\n  slack: ['#x']\n")
	if err := r.Validate(unknown); err == nil || !strings.Contains(err.Error(), `"slack"`) {
		t.Errorf("a kind with no target: err = %v", err)
	}
}

func TestRegistry_CoreShipsWebhookAndEmail(t *testing.T) {
	r := NewRegistry(nil)
	for _, kind := range []string{"webhook", "email"} {
		if _, ok := r.Lookup(kind); !ok {
			t.Errorf("the core registry has no %s target", kind)
		}
	}
	if _, ok := r.Lookup("slack"); ok {
		t.Error("the core registry sends slack")
	}
}

func TestRegistry_AGivenEmailReplacesTheCores(t *testing.T) {
	mine := EmailTarget{SMTP: &SMTP{Host: "smtp.example.test"}}
	got, _ := NewRegistry(nil, mine).Lookup("email")
	if got.(EmailTarget).SMTP != mine.SMTP {
		t.Error("the core's email target was kept over the one given")
	}
}

func TestRegistry_RegisteringAKindTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a second webhook target was accepted")
		}
	}()
	NewRegistry(nil, NewWebhookTarget(nil))
}
