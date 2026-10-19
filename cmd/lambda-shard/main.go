package main

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambdaurl"
)

func main() {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
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
	weftsdk.Infof(context.Background(), "lambda-shard: listening on :%s", port)
	http.HandleFunc("/", ServeHTTP)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
