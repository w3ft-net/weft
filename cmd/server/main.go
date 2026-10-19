package main

import (
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"log"
	"net/http"
	"os"
)

func main() {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	weftsdk.Infof(context.Background(), "weft starting on :%s", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", port), nil); err != nil {
		log.Fatal(err)
	}
}
