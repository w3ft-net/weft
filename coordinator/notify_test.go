package coordinator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/alerting"
)

// fakeTarget is a kind the core has never heard of, added through the registry.
type fakeTarget struct {
	got  []alerting.Alert
	fail error
}

func (*fakeTarget) Kind() string { return "carrier-pigeon" }

func (f *fakeTarget) Open(entry *yaml.Node) (alerting.Destination, error) {
	var t struct {
		Coop string `yaml:"coop"`
	}
	if entry.Kind == yaml.ScalarNode {
		t.Coop = entry.Value
	} else if err := entry.Decode(&t); err != nil {
		return alerting.Destination{}, err
	}
	if t.Coop == "" {
		return alerting.Destination{}, errors.New("carrier-pigeon target missing `coop`")
	}
	return alerting.Destination{
		Key:  "carrier-pigeon:" + t.Coop,
		Name: t.Coop,
		Send: func(_ context.Context, a alerting.Alert) error {
			f.got = append(f.got, a)
			return f.fail
		},
	}, nil
}

// An adapter adds a kind with Register alone: the alias key is parsed by the
// store without knowing it, the notifier hands each entry (bare or mapping) to
// the target, and the delivered map skips a destination already reached.
func TestNotifier_RegisteredKindIsDispatched(t *testing.T) {
	env := newWFEnv(t)
	yaml := "system:\n  email:\n    - ops@example.test\nflock:\n  carrier-pigeon:\n    - north\n    - coop: south\n"
	if err := env.notify.Save(env.owner, "", yaml); err != nil {
		t.Fatal(err)
	}
	f := &fakeTarget{}
	n := NewNotifier(env.notify, f)

	sent, err := n.Send(context.Background(), env.owner, "flock", "disk", "boom", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := []string{"carrier-pigeon:north", "carrier-pigeon:south"}; !slices.Equal(sent, want) {
		t.Errorf("sent = %v, want %v", sent, want)
	}
	if len(f.got) != 2 || f.got[0] != (alerting.Alert{Owner: env.owner, Alias: "flock", Cron: "disk", Body: "boom"}) {
		t.Errorf("target saw %+v", f.got)
	}

	f.got = nil
	sent, err = n.Send(context.Background(), env.owner, "flock", "disk", "boom", map[string]bool{"carrier-pigeon:north": true})
	if err != nil || !slices.Equal(sent, []string{"carrier-pigeon:south"}) || len(f.got) != 1 {
		t.Errorf("retry: sent = %v, err = %v, sends = %d; want only south", sent, err, len(f.got))
	}
}

// A destination that fails is reported and the others still go out.
func TestNotifier_OneTargetFailingDoesNotStopTheRest(t *testing.T) {
	env := newWFEnv(t)
	if err := env.notify.Save(env.owner, "", "system:\n  carrier-pigeon: [a]\nflock:\n  carrier-pigeon: [a, b]\n"); err != nil {
		t.Fatal(err)
	}
	f := &fakeTarget{fail: errors.New("storm")}
	sent, err := NewNotifier(env.notify, f).Send(context.Background(), env.owner, "flock", "disk", "boom", nil)
	if err == nil || err.Error() != "storm" || len(sent) != 0 || len(f.got) != 2 {
		t.Errorf("sent = %v, err = %v, sends = %d; want both tried and the first error", sent, err, len(f.got))
	}
}

// A kind with no registered target is an error: the alert reached nobody it
// names there. The kinds that are registered still go out, and their delivered
// keys come back so the retry skips them.
func TestNotifier_UnregisteredKindIsAnError(t *testing.T) {
	env := newWFEnv(t)
	if err := env.notify.Save(env.owner, "", "system:\n  email: [a@example.test]\nflock:\n  smoke-signal: [hill]\n  carrier-pigeon: [north]\n"); err != nil {
		t.Fatal(err)
	}
	f := &fakeTarget{}
	sent, err := NewNotifier(env.notify, f).Send(context.Background(), env.owner, "flock", "disk", "boom", nil)
	if err == nil || !strings.Contains(err.Error(), `"smoke-signal"`) {
		t.Errorf("err = %v, want one naming the missing smoke-signal target", err)
	}
	if !slices.Equal(sent, []string{"carrier-pigeon:north"}) {
		t.Errorf("sent = %v, want the registered kind still delivered", sent)
	}
}

// A webhook target's ${secrets.NAME} references are filled in at send time:
// the endpoint sees the real values, and nothing that comes back out of Send
// (an error carries the request URL) does.
func TestSendWebhookInterpolatesSecrets(t *testing.T) {
	env := newWFEnv(t)
	var gotAuth, gotKey string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.URL.Query().Get("key")
		if fail {
			http.Error(w, "no", 500)
		}
	}))
	defer srv.Close()

	for name, v := range map[string]string{"token": "tok-123", "api_key": "key-456"} {
		if err := env.notify.SetSecret(env.owner, name, v); err != nil {
			t.Fatal(err)
		}
	}
	yaml := "system:\n  email:\n    - ops@example.test\nhook:\n  webhook:\n    - url: \"" + srv.URL + "/in?key=${secrets.api_key}\"\n      headers:\n        Authorization: \"Bearer ${secrets.token}\"\n"
	if err := env.notify.Save(env.owner, "", yaml); err != nil {
		t.Fatal(err)
	}

	n := NewNotifier(env.notify)
	if _, err := n.Send(context.Background(), env.owner, "hook", "disk", "boom", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tok-123" || gotKey != "key-456" {
		t.Errorf("endpoint saw Authorization=%q key=%q, want the secrets filled in", gotAuth, gotKey)
	}

	fail = true
	_, err := n.Send(context.Background(), env.owner, "hook", "disk", "boom", nil)
	if err == nil {
		t.Fatal("a 500 was not an error")
	}
	if strings.Contains(err.Error(), "key-456") || strings.Contains(err.Error(), "tok-123") {
		t.Errorf("error leaks a secret: %v", err)
	}

	// A refused connection is the error that quotes the whole URL.
	srv.Close()
	_, err = n.Send(context.Background(), env.owner, "hook", "disk", "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "${secrets.api_key}") || strings.Contains(err.Error(), "key-456") {
		t.Errorf("connection error = %v, want the URL shown with its placeholder, not the secret", err)
	}
}
