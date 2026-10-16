package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/graybeardcorp/weft/internal/weftlog"
)

func main() {
	weftlog.Init(os.Stderr)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	weftlog.Infof(context.Background(), "weft starting on :%s", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", port), nil); err != nil {
		log.Fatal(err)
	}
}
