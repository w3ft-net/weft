package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
)

// Notifier dispatches alerts through the per-customer alias map stored in
// /etc/notify.yaml. A cron watch's `notify:` field names an alias; the alias
// fans out to its targets, each handled by the AlertTarget registered for its
// kind. The core ships webhook and email; a deployment registers the rest.
type Notifier struct {
	Store *store.Store
	// Timeout is how long one target gets to accept a notification, whatever
	// kind of target it is. Zero means alerting.Timeout; tests shrink it.
	Timeout time.Duration
	// Targets are the kinds this build sends. A save of /etc/notify.yaml is
	// validated against the same registry.
	Targets *alerting.Registry
}

// ErrDeliveryTimeout marks a Send in which at least one target did not accept
// the notification in time. A cron whose deliveries keep timing out is benched
// the same way as one whose runs do (see CronExecutor.executeWatch).
var ErrDeliveryTimeout = errors.New("delivery timed out")

// deliveryTimeoutError is the first failure of a Send that also timed out,
// keeping its message while answering errors.Is(err, ErrDeliveryTimeout).
type deliveryTimeoutError struct{ err error }

func (e *deliveryTimeoutError) Error() string        { return e.err.Error() }
func (e *deliveryTimeoutError) Unwrap() error        { return e.err }
func (e *deliveryTimeoutError) Is(target error) bool { return target == ErrDeliveryTimeout }

// deliver runs one target's send under the delivery budget. The budget is its
// own: it neither inherits the cron run's deadline nor depends on the send
// honoring its context (SMTP doesn't), so a stuck target is abandoned and
// reported as ErrDeliveryTimeout whatever it is.
func (n *Notifier) deliver(ctx context.Context, send func(context.Context) error) error {
	budget := n.Timeout
	if budget == 0 {
		budget = alerting.Timeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- send(ctx) }()
	select {
	case err := <-done:
		var ne net.Error
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())) {
			return fmt.Errorf("%w after %s: %v", ErrDeliveryTimeout, budget, err)
		}
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w after %s", ErrDeliveryTimeout, budget)
	}
}

// NewNotifier creates a notifier over the core's kinds (alerting.NewRegistry)
// and any others given.
func NewNotifier(s *store.Store, targets ...alerting.AlertTarget) *Notifier {
	return &Notifier{Store: s, Targets: alerting.NewRegistry(s, targets...)}
}

// Send dispatches a notification to the named alias from the customer's
// /etc/notify.yaml. watchName is the cron's identity, which a target may use as
// a dedup key so a watch firing repeatedly during an outage collapses to one
// incident — ad-hoc shell invocations should pass a fresh synthetic name (see
// AlertContext.dispatchIdentity). body is the alert text; individual
// targets may truncate further to satisfy per-service limits.
//
// Targets whose key is in delivered are skipped: they already heard about
// this incident. Send returns the keys of the targets it reached, even when
// it also returns an error for the ones it didn't, so the caller can record
// the first without repeating it on the retry.
func (n *Notifier) Send(ctx context.Context, orgID store.OrgID, aliasName, watchName, body string, delivered map[string]bool) ([]string, error) {
	aliasName = strings.TrimSpace(aliasName)
	if aliasName == "" {
		return nil, nil
	}

	nc, err := n.Store.GetNotifyConfig(orgID)
	if err != nil {
		return nil, fmt.Errorf("no notify.yaml configured")
	}
	aliases, err := store.ParseNotifyAliases(nc.AliasesYAML)
	if err != nil {
		return nil, fmt.Errorf("parse notify.yaml: %w", err)
	}
	alias, ok := aliases[aliasName]
	if !ok {
		return nil, fmt.Errorf("alias %q not defined in /etc/notify.yaml", aliasName)
	}

	alert := alerting.Alert{Org: orgID, Alias: aliasName, Cron: watchName, Body: body}
	var sent []string
	var firstErr error
	timedOut := false
	fail := func(kind, name string, err error) {
		if errors.Is(err, ErrDeliveryTimeout) {
			timedOut = true
		}
		weftsdk.Infof(ctx, "notify[%s]: %s %s: %v", watchName, kind, name, err)
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, k := range alias.Targets() {
		target, ok := n.Targets.Lookup(k.Kind)
		if !ok {
			// A kind this build never sends is a failure, not a skip: the alert
			// reached nobody it names. The kinds that are registered still go out.
			fail(k.Kind, "(unregistered)", fmt.Errorf("alias %q has %s targets but this build has no %q alert target", aliasName, k.Kind, k.Kind))
			continue
		}
		for _, entry := range k.Entries {
			dest, err := target.Open(entry)
			if err != nil {
				fail(k.Kind, "(invalid)", err)
				continue
			}
			if delivered[dest.Key] {
				continue
			}
			if err := n.deliver(ctx, func(ctx context.Context) error { return dest.Send(ctx, alert) }); err != nil {
				fail(k.Kind, dest.Name, err)
				continue
			}
			sent = append(sent, dest.Key)
		}
	}
	if timedOut && !errors.Is(firstErr, ErrDeliveryTimeout) {
		firstErr = &deliveryTimeoutError{firstErr}
	}
	return sent, firstErr
}
