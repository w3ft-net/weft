package main

import (
	"net/http"
)

func main() {
	// In Lambda, this would use lambda.StartWithOptions with response streaming.
	// For local dev/testing, run as a plain HTTP server.
	http.HandleFunc("/", ServeHTTP)
	http.ListenAndServe(":9030", nil)
}
