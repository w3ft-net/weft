// Command api is hosted weft's HTTP api. The process itself is package
// app/apiapp; this main supplies the hosted adapter.
package main

import (
	"github.com/graybeardcorp/weft/app/apiapp"
	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapi"
)

func main() { apiapp.API(hostedapi.API()) }
