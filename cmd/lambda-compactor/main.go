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
		lambdaurl.Start(http.HandlerFunc(ServeHTTP))
		return
	}

	// Local dev / Docker / RIE: plain HTTP server.
	port := os.Getenv("PORT")
	if port == "" {
		port = "9040"
	}
	weftlog.Infof(context.Background(), "lambda-compactor: listening on :%s", port)
	http.HandleFunc("/", ServeHTTP)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
