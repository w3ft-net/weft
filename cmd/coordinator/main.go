// Command coordinator is hosted weft's coordinator. The process itself is
// package app; this main supplies the hosted adapter.
package main

import (
	"github.com/graybeardcorp/weft/app"
	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapp"
)

func main() { app.Coordinator(hostedapp.Coordinator()) }
