package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/user"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/w3ft-net/weft/cmd/weft/runner"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/shell"
	"github.com/w3ft-net/weft/timerange"
)

func shCmd(e Env) *cobra.Command {
	var pipeline, rangeSpec string
	cmd := &cobra.Command{
		Use:   "sh",
		Short: "Start the interactive shell, or run one pipeline with -c",
		Example: "  weft sh\n" +
			"  weft sh -c 'cat /log/myco/prod/api/main.log | grep ERROR | wc -l'\n" +
			"  weft sh --range 1h.. -c 'cat /log/myco/prod/api/main.log'",
		Args: cobra.NoArgs,
		RunE: runE(func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("range") && !cmd.Flags().Changed("command") {
				return fmt.Errorf("--range applies to a pipeline: pass it with -c")
			}
			if rangeSpec != "" {
				// Validate locally so a typo errors out before the round trip;
				// the server is the canonical resolver.
				if _, _, err := timerange.ParseRange(rangeSpec); err != nil {
					return fmt.Errorf("invalid --range: %v", err)
				}
			}
			s, err := e.Shell()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("command") {
				return runOne(s, pipeline, rangeSpec)
			}
			return runInteractive(e, s)
		}),
	}
	cmd.Flags().StringVarP(&pipeline, "command", "c", "", "Run this pipeline and exit")
	cmd.Flags().StringVar(&rangeSpec, "range", "", "Time range for the pipeline, e.g. 1h.. or 2d..1d (with -c)")
	return cmd
}

func runOne(s *shell.Shell, pipeline, rangeSpec string) error {
	if rangeSpec != "" {
		s.Range = rangeSpec
	}
	// Only meaningful for `trace` output (ExecNonInteractive's doc on
	// Shell.TermWidth): stdout not being a terminal is not an error, it falls
	// back to the field's zero value.
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		s.TermWidth = w
	}
	if err := s.ExecNonInteractive(pipeline, os.Stdout); err != nil {
		return fmt.Errorf("error: %v", err)
	}
	return nil
}

func runInteractive(e Env, s *shell.Shell) error {
	// The prompt's identity comes from the deployment, when it has one. Fall
	// through to the OS user if the lookup fails so a network blip never blocks
	// the shell from starting.
	if id := e.(*env).opts.Identity; id != nil && (s.User == "" || s.Customer == "") {
		if cfg, err := e.Config(); err == nil {
			if cred, err := e.Credential(cfg); err == nil {
				if u, g, ok := id(cfg.Endpoint, cred, e.Org(cfg)); ok {
					if s.User == "" {
						s.User = u
					}
					if s.Customer == "" {
						s.Customer = g
					}
				}
			}
		}
	}
	if s.User == "" {
		if u, err := user.Current(); err == nil {
			s.User = u.Username
		}
	}

	// If stdin is not a terminal, run its lines non-interactively.
	if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice == 0 {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if err := s.ExecNonInteractive(scanner.Text(), os.Stdout); err != nil {
				ui.Errorf("error: %v\n", err)
			}
		}
		return nil
	}
	if err := runner.Run(s); err != nil {
		return fmt.Errorf("shell error: %v", err)
	}
	return nil
}
