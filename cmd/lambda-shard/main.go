package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambdaurl"
	"github.com/graybeardcorp/weft/internal/weftlog"
)

func main() {
	weftlog.Init(os.Stderr)
	// AWS_LAMBDA_RUNTIME_API is set by the Lambda runtime automatically.
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		// Lambda mode: lambdaurl.Start wraps ServeHTTP as a function URL handler.
		// Response streaming happens automatically when the function URL is
		// configured with RESPONSE_STREAM invoke mode.
		lambdaurl.Start(http.HandlerFunc(ServeHTTP))
		return
	}

	// Local dev / Docker / RIE: plain HTTP server.
	port := os.Getenv("PORT")
	if port == "" {
		port = "9030"
	}
	weftlog.Infof(context.Background(), "lambda-shard: listening on :%s", port)
	http.HandleFunc("/", ServeHTTP)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
