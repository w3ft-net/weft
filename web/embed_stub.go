//go:build !webembed

package web

import "net/http"

// Handler is nil: this binary does not carry the web app (build with -tags
// webembed after `make web`).
func Handler() http.Handler { return nil }
