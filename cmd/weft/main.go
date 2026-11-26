// Command weft is the open-source weft CLI: the interactive shell and the
// commands that manage an open-core deployment, authenticating with its one
// HTTP Basic credential.
package main

import "github.com/w3ft-net/weft/cmd/weft/cli"

// Version is the released semver; -ldflags "-X main.Version=..." overrides it.
var Version = "dev"

func main() {
	root, _ := cli.NewRoot(cli.Options{ClientID: "weft/" + Version})
	cli.Run(root)
}
