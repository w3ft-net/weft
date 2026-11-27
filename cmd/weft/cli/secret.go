package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/w3ft-net/weft/cmd/weft/ui"
)

// secretCmd manages the named secrets alert targets deliver with: a webhook's
// ${secrets.NAME}, `slack_token`, `pagerduty_<name>`. Values go in and never
// come back out.
func secretCmd(e Env) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Manage the secrets alert targets use (set, rm, list)"}

	var value string
	set := &cobra.Command{
		Use: "set NAME [--value VALUE]", Short: "Create or replace a secret", Args: cobra.ExactArgs(1),
		Long: "Create or replace a secret. Without --value the value is read from standard input.",
		RunE: runE(func(_ *cobra.Command, args []string) error {
			if value == "" {
				if term.IsTerminal(int(os.Stdin.Fd())) {
					return errors.New("give the value with --value, or pipe it on standard input")
				}
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				value = strings.TrimRight(string(b), "\r\n")
			}
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := client.SaveSecret(args[0], value); err != nil {
				return fmt.Errorf("set secret: %v", err)
			}
			ui.Successf("Set secret: %s\n", args[0])
			return nil
		}),
	}
	set.Flags().StringVar(&value, "value", "", "The secret's value")

	rm := &cobra.Command{
		Use: "rm NAME", Aliases: []string{"remove"}, Short: "Remove a secret that no alias refers to", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := client.DeleteSecret(args[0]); err != nil {
				return fmt.Errorf("remove secret: %v", err)
			}
			ui.Successf("Removed secret: %s\n", args[0])
			return nil
		}),
	}

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List secret names", Args: cobra.NoArgs,
		RunE: runE(func(cmd *cobra.Command, _ []string) error {
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			names, err := client.ListSecrets()
			if err != nil {
				return fmt.Errorf("list secrets: %v", err)
			}
			if len(names) == 0 {
				ui.Hintln("No secrets.")
				return nil
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		}),
	}

	cmd.AddCommand(set, rm, list)
	return cmd
}
