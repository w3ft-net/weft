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
		lambdaurl.Start(http.HandlerFunc(ServeHTTP))
		return
	}

	// Local dev / Docker / RIE: plain HTTP server.
	port := os.Getenv("PORT")
	if port == "" {
		port = "9040"
	}
	weftsdk.Infof(context.Background(), "lambda-compactor: listening on :%s", port)
	http.HandleFunc("/", ServeHTTP)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
