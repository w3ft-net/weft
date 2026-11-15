package warning

import (
	"errors"
	"fmt"
)

// Warning is a non-blocking, user-facing message. A save/validate
// function returns one (bare, or joined with others via errors.Join)
// to mean "this succeeded, but here's something worth telling the
// user" — as opposed to a plain error, which means the operation was
// rejected. Every save surface renders whatever error comes back
// through Split; none of them need to know which check produced a
// given Warning, or why.
type Warning struct{ msg string }

// New builds a Warning from a formatted message.
func New(format string, args ...any) *Warning {
	return &Warning{msg: fmt.Sprintf(format, args...)}
}

func (w *Warning) Error() string { return w.msg }

// Split walks err — nil, a plain error, a *Warning, or an errors.Join
// tree mixing both — into blocking errors and warning messages.
// Callers reject the operation iff len(blocking) > 0; they display
// warnings unconditionally (nil/empty when there are none, regardless
// of whether the operation was rejected).
func Split(err error) (blocking []error, warnings []string) {
	if err == nil {
		return nil, nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			b, w := Split(e)
			blocking = append(blocking, b...)
			warnings = append(warnings, w...)
		}
		return blocking, warnings
	}
	var warn *Warning
	if errors.As(err, &warn) {
		return nil, []string{warn.Error()}
	}
	return []error{err}, nil
}
