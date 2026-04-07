package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9030"
	}

	// AWS_LAMBDA_RUNTIME_API is set automatically by the Lambda runtime.
	// When present, we're running in Lambda. Otherwise, plain HTTP server.
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		// Lambda mode — response streaming would be wired here.
		// For now, the Lambda adapter would call ServeHTTP internally.
		// TODO: wire lambda.StartWithOptions with response streaming
		log.Fatal("Lambda runtime not yet wired — run as HTTP server instead")
	}

	log.Printf("lambda-shard: listening on :%s", port)
	http.HandleFunc("/", ServeHTTP)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
