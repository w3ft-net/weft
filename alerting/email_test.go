package alerting

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEmail_NoConfigFailsToSend(t *testing.T) {
	var n yaml.Node
	if err := n.Encode("test@example.com"); err != nil {
		t.Fatal(err)
	}
	d, err := EmailTarget{}.Open(&n)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), Alert{Cron: "error-rate", Body: "test body"}); err == nil {
		t.Fatal("expected error when no email config")
	}
}
