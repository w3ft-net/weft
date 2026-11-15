package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/notifycfg"
)

func TestNotifierDeliverTimesOutAnyTarget(t *testing.T) {
	n := NewNotifier(notifycfg.Service{})
	n.Timeout = 50 * time.Millisecond
	err := n.deliver(context.Background(), func(ctx context.Context) error {
		time.Sleep(time.Second) // a send that ignores its ctx, like SMTP
		return nil
	})
	if !errors.Is(err, ErrDeliveryTimeout) {
		t.Fatalf("err = %v, want ErrDeliveryTimeout", err)
	}
	if err := n.deliver(context.Background(), func(ctx context.Context) error { return errors.New("boom") }); errors.Is(err, ErrDeliveryTimeout) {
		t.Fatalf("an ordinary failure was reported as a timeout: %v", err)
	}
}
