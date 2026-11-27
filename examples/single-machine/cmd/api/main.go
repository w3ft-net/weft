// Command api is the open-source weft HTTP api: one group, one HTTP Basic
// credential, SQLite stores. The process itself is package server
// (cmd/api/server); this main supplies the open deployment. It imports no
// coordinator, so the binary is built without cgo.
package main

import (
	"log"

	"github.com/w3ft-net/weft/cmd/api/server"
	"github.com/w3ft-net/weft/deployment/local"
)

func main() {
	fl := server.ParseFlags()
	open, closeOpen, err := local.Open(fl.DBDSN, local.WithBaseURL(fl.BaseURL))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer closeOpen()
	server.Run(fl, open.Deployment, &server.Own{})
}
