package shell

import "net/http"

// A RequestDecorator adds what a deployment needs on every request the clients
// send, beyond the credential: an org selector, a tenant header, a trace tag.
// Core's clients know no deployment's headers; the binary supplies decorators.
type RequestDecorator func(*http.Request)

// Header is a decorator that sets one header, and does nothing when the value is
// empty.
func Header(name, value string) RequestDecorator {
	return func(r *http.Request) {
		if value != "" {
			r.Header.Set(name, value)
		}
	}
}

func decorate(r *http.Request, ds []RequestDecorator) {
	for _, d := range ds {
		d(r)
	}
}
