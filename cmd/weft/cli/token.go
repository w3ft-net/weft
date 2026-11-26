package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/w3ft-net/weft/cmd/weft/ui"
)

func tokenCmd(e Env) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage agent tokens (create, list, revoke)"}

	var label, env string
	create := &cobra.Command{
		Use: "create NS", Short: "Create an agent token for a namespace", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			ns, err := namespaceArg(args[0])
			if err != nil {
				return err
			}
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			token, err := client.CreateToken("", label, ns, env)
			if err != nil {
				return fmt.Errorf("create token: %v", err)
			}
			st := ui.Out()
			fmt.Println(st.KV("Token created", 13, st.Name.Render(token)))
			if label != "" {
				fmt.Println(st.KV("Label", 13, label))
			}
			if env != "" {
				fmt.Println(st.KV("Env", 13, env+st.Muted.Render(" (stamped on every row from this token)")))
			}
			ui.Warnf("Store this token somewhere safe. It cannot be retrieved again.\n")
			return nil
		}),
	}
	create.Flags().StringVar(&label, "label", "", "A name for the token")
	create.Flags().StringVar(&env, "env", "", "Stamp every row from this token with this environment")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List agent tokens", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			tokens, err := client.ListTokens("")
			if err != nil {
				return fmt.Errorf("list tokens: %v", err)
			}
			if len(tokens) == 0 {
				ui.Hintln("No tokens configured.")
				return nil
			}
			// The raw token isn't available after creation; the opaque ID is what
			// `weft token revoke --id` takes.
			rows := make([][]string, 0, len(tokens))
			for _, t := range tokens {
				env := t.Env
				if env == "" {
					env = "-"
				}
				rows = append(rows, []string{t.ID, t.Namespace, env, t.Label, t.Status, t.CreatedAt})
			}
			ui.WriteTable(cmd.OutOrStdout(), ui.Out(), "", []ui.Column{
				ui.MutedCol("ID", 20), ui.MutedCol("NAMESPACE", 15), ui.MutedCol("ENV", 15), ui.NameCol("LABEL", 25),
				ui.StatusCol("STATUS", 10), {Title: "CREATED"},
			}, rows)
			return nil
		}),
	})

	var id string
	revoke := &cobra.Command{
		Use: "revoke --id ID", Short: "Revoke an agent token (see `weft token list`)", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			client, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := client.RevokeToken(id); err != nil {
				return fmt.Errorf("revoke token: %v", err)
			}
			ui.Successf("Revoked token %s\n", id)
			return nil
		}),
	}
	revoke.Flags().StringVar(&id, "id", "", "The token's ID, from `weft token list`")
	revoke.MarkFlagRequired("id")
	cmd.AddCommand(revoke)
	return cmd
}
