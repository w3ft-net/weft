package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/graybeardcorp/weft/planner"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: explain '<pipeline>'\n")
		os.Exit(1)
	}
	pipeline := strings.Join(os.Args[1:], " ")
	fmt.Print(planner.Explain(pipeline))
}
