package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/shell"
)

func namespaceCmd(e Env) *cobra.Command {
	cmd := &cobra.Command{Use: "namespace", Aliases: []string{"ns"}, Short: "Manage namespaces (list, create, remove)"}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List namespaces", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			nss, err := remote.ListNamespaces()
			if err != nil {
				return fmt.Errorf("list namespaces: %v", err)
			}
			if len(nss) == 0 {
				ui.Hintln("No namespaces. Create one with: weft namespace create <name>")
				return nil
			}
			for _, ns := range nss {
				fmt.Println(ui.Out().Name.Render(ns.Name))
			}
			return nil
		}),
	})

	cmd.AddCommand(&cobra.Command{
		Use: "create NAME", Short: "Create a namespace", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			name, err := namespaceArg(args[0])
			if err != nil {
				return err
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			ns, err := remote.CreateNamespace(name)
			if err != nil {
				return fmt.Errorf("create namespace: %v", err)
			}
			ui.Successf("Created namespace: %s\n", ns.Name)
			return nil
		}),
	})

	var force bool
	remove := &cobra.Command{
		Use: "remove NS", Aliases: []string{"rm"}, Short: "Remove a namespace", Args: cobra.ExactArgs(1),
		RunE: runE(func(_ *cobra.Command, args []string) error {
			name, err := namespaceArg(args[0])
			if err != nil {
				return err
			}
			remote, err := e.Coordinator()
			if err != nil {
				return err
			}
			if err := remote.RemoveNamespace(name, force); err != nil {
				var notEmpty *shell.NamespaceNotEmptyError
				if errors.As(err, &notEmpty) {
					ui.Errorf("namespace %s is not empty:\n", name)
					for _, s := range notEmpty.Sources {
						ui.Errorf("  source %s/%s\n", name, s)
					}
					if notEmpty.Tokens > 0 {
						ui.Errorf("  %d live token(s)\n", notEmpty.Tokens)
					}
					ui.Errorf("Remove them first, or re-run with --force to delete the namespace and all of it:\n    weft namespace remove %s --force\n", name)
					return errReported
				}
				return fmt.Errorf("remove namespace: %v", err)
			}
			ui.Successf("Removed namespace: %s\n", name)
			return nil
		}),
	}
	remove.Flags().BoolVar(&force, "force", false, "Delete the namespace with its sources and tokens")
	cmd.AddCommand(remove)
	return cmd
}
