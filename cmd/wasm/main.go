//go:build js && wasm

// Command wasm builds weft's browser shell. The shell itself is package boot.
package main

import "github.com/w3ft-net/weft/cmd/wasm/boot"

func main() { boot.Main(boot.Options{}) }
