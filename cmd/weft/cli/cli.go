// Package cli is the weft command line: a cobra tree built by NewRoot over an
// Env. A binary's main chooses what the tree holds. It takes the core commands
// NewRoot registers, adds its own with AddCommand, and picks the AuthProvider
// that says how the client proves who it is. Core ships Basic, the open build's
// single credential; a deployment with accounts brings its own provider (and the
// login command that goes with it) the same way.
package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/shell"
)

// Options is what a binary decides about its CLI.
type Options struct {
	// ClientID is "<binary>/<version>", sent as X-Weft-Client and printed by
	// `version`.
	ClientID string

	// Auth says how requests are authenticated. Nil means Basic.
	Auth AuthProvider

	// Tiers fills the plan section of the shell's `help cron`; nil leaves it out.
	Tiers shell.TierLimitsProvider

	// Identity names the signed-in user for the shell prompt. Nil, or a false
	// result, falls back to the OS user.
	Identity func(endpoint, credential string, decorators []shell.RequestDecorator) (user, group string, ok bool)

	// Decorators adds the deployment's own headers (an active-org selector, say)
	// to every request the clients send. Nil adds none.
	Decorators func(cfg *shell.Config) []shell.RequestDecorator

	// PreRun runs before every command, once its flags are parsed.
	PreRun func(cmd *cobra.Command, args []string)
}

// AuthProvider says how this client authenticates to the api.
type AuthProvider interface {
	// Credential is what the client presents: a bare token (sent as a bearer
	// token) or a full Authorization value that names its scheme ("Basic ...").
	// Its error is shown to the user as it is, so it should say what to do.
	Credential(cfg *shell.Config) (string, error)
}

// Env is what a command can reach: the configuration, the credential and the
// api clients built from them. Nothing in it is specific to one deployment.
type Env interface {
	// Config is the loaded configuration with the --org flag applied.
	Config() (*shell.Config, error)
	SaveConfig(*shell.Config) error
	// Decorators are the deployment's request decorators for cfg.
	Decorators(cfg *shell.Config) []shell.RequestDecorator
	// Credential is the provider's credential for cfg.
	Credential(cfg *shell.Config) (string, error)
	// Coordinator is an authenticated client for the api.
	Coordinator() (*shell.HTTPCoordinator, error)
	// Backend is the same, for the config endpoints (scripts, crons, dashboards).
	Backend() (*shell.HTTPBackend, error)
	// Shell is a shell wired to the api, ready for a query or the interactive loop.
	Shell() (*shell.Shell, error)
	ClientID() string
}

// NewRoot is the weft command with the core commands under it, and the Env they
// share. A bare `weft` lists the subcommands.
func NewRoot(opts Options) (*cobra.Command, Env) {
	if opts.Auth == nil {
		opts.Auth = Basic{}
	}
	env := &env{opts: opts}

	root := &cobra.Command{
		Use:           "weft",
		Short:         "Log analysis for your Unix heart",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if opts.PreRun != nil {
				opts.PreRun(cmd, args)
			}
		},
	}
	cobra.EnableTraverseRunHooks = true
	root.PersistentFlags().BoolVar(&ui.NoColor, "nocolor", false, "Disable colored output (also honors NO_COLOR; off when piped)")
	root.SetGlobalNormalizationFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "no-color" {
			return "nocolor"
		}
		return pflag.NormalizedName(name)
	})
	root.AddCommand(
		shCmd(env), namespaceCmd(env), sourceCmd(env), tokenCmd(env),
		mcpCmd(env), proposeCmd(env), versionCmd(env),
	)
	return root, env
}

// Run executes root and exits 1 with the error, styled, if a command fails.
func Run(root *cobra.Command) {
	if err := root.Execute(); err != nil {
		if !errors.Is(err, errReported) {
			ui.Errorln(err)
		}
		os.Exit(1)
	}
}

// errReported marks an error whose text the command already printed.
var errReported = errors.New("reported")

// runE makes a command's failure plain (no usage dump), since the arguments
// parsed: usage is for argument errors only.
func runE(f func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return f(cmd, args)
	}
}

// parseTarget splits "ns/svc". The service may itself contain a slash
// ("weft/shard/main"), so only the first one separates.
func parseTarget(arg string) (namespace, service string, err error) {
	ns, svc, ok := strings.Cut(arg, "/")
	if !ok || ns == "" || svc == "" {
		return "", "", fmt.Errorf("%q is not a source: write it as ns/svc", arg)
	}
	return ns, svc, nil
}

// namespaceArg is a namespace, which has no slash.
func namespaceArg(arg string) (string, error) {
	if arg == "" || strings.Contains(arg, "/") {
		return "", fmt.Errorf("%q is not a namespace: write it as ns", arg)
	}
	return arg, nil
}
