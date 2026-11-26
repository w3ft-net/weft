package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func versionCmd(e Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the client version",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), e.ClientID()) },
	}
}
