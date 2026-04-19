package api

import "net/http"

// BodyCap returns middleware that caps r.Body to maxBytes before the
// handler runs. Reading past the cap makes r.Body.Read return an
// error whose .Error() contains "http: request body too large"; the
// json decoder surfaces this through its own error path, and our
// handlers already return 400 on decode errors — but we want a clean
// 413 instead, so we check the size up front via Content-Length and
// short-circuit. For requests without Content-Length (chunked), the
// MaxBytesReader enforcement still runs and the handler will fail
// gracefully.
func BodyCap(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// BodyCapFunc is the handlerfunc-composable flavor.
func BodyCapFunc(maxBytes int64, next http.HandlerFunc) http.HandlerFunc {
	h := BodyCap(maxBytes, next)
	return func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }
}
