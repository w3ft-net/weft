package logreads

import "errors"

func errorsAs(err error, target **UnresolvableError) bool { return errors.As(err, target) }
