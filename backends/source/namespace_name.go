package source

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ValidateNamespaceName reports whether name can be a single namespace.
// A namespace is one path segment (/log/<ns>/...), a token scope, and the
// stem of the "<ns>-r" and "<ns>-w" permission groups, so a name that holds a list
// separator (comma), whitespace, a path slash, the "+" facet marker, a glob
// character, a control character, or that starts like a flag could never
// round-trip through those and is refused up front.
func ValidateNamespaceName(name string) error {
	if name == "" {
		return errors.New("namespace name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid namespace name %q: must not start with '-'", name)
	}
	for _, r := range name {
		switch {
		case r == ',' || r == '/' || r == '+' || r == '*' || r == '?' || r == '[':
			return fmt.Errorf("invalid namespace name %q: must not contain %q", name, string(r))
		case unicode.IsSpace(r) || unicode.IsControl(r):
			return fmt.Errorf("invalid namespace name %q: must not contain whitespace or control characters", name)
		}
	}
	return nil
}
