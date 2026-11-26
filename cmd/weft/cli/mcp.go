package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/w3ft-net/weft/cmd/weft/mcp"
	"github.com/w3ft-net/weft/cmd/weft/mcp/proposals"
	"github.com/w3ft-net/weft/shell"
	"github.com/w3ft-net/weft/timerange"
)

// mcpCmd starts the weft MCP server on stdio. It reuses the same auth and
// connection path as `weft sh` and surfaces the resulting Shell and ProxyFS to
// the SDK as the seam underneath weft.run, weft.ls and weft.cat.
//
// Stdio is the right transport for the local-LLM use case (Claude Desktop,
// Cursor and Claude Code all spawn MCP servers as subprocesses and talk
// newline-delimited JSON over stdin/stdout). Anything printed to stderr is
// invisible to the protocol but shows up in the host's MCP server logs.
func mcpCmd(e Env) *cobra.Command {
	return &cobra.Command{
		Use: "mcp", Short: "Start an MCP server on stdio for LLM clients", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			s, err := e.Shell()
			if err != nil {
				return err
			}
			mcpCfg := mcp.Config{
				Run: func(pipeline string, rangeSpec string, w io.Writer) error {
					if rangeSpec != "" {
						if _, _, err := timerange.ParseRange(rangeSpec); err != nil {
							return fmt.Errorf("invalid range %q: %w", rangeSpec, err)
						}
						s.Range = rangeSpec
					}
					return s.ExecNonInteractive(pipeline, w)
				},
				Ls:  fsLs(s),
				Cat: fsCat(s),
			}

			// Wire propose/apply against the same on-disk staging dir `weft propose
			// review/apply/clear` reads. If the user can't reach a writable dir
			// (read-only HOME, exotic env) skip the propose tools rather than
			// failing the whole MCP startup.
			if dir, err := defaultProposalsDir(); err == nil {
				if store, err := proposals.New(dir); err == nil {
					mcpCfg.Proposals = store
					backend, err := e.Backend()
					if err != nil {
						return err
					}
					wireProposalBackend(&mcpCfg, backend)
				} else {
					fmt.Fprintf(os.Stderr, "warning: propose tools disabled — could not open %s: %v\n", dir, err)
				}
			}

			// server.Run blocks until the client closes the connection or sends a
			// shutdown.
			if err := mcp.NewServer(mcpCfg).Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil && err != context.Canceled {
				return fmt.Errorf("mcp server: %v", err)
			}
			return nil
		}),
	}
}

// defaultProposalsDir is ~/.weft/proposals/current. The MCP server and `weft
// propose` agree on this path; running several MCP servers at once is not
// supported (the second would share the first's staging area).
func defaultProposalsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weft", "proposals", "current"), nil
}

// fsLs and fsCat adapt the shell's ProxyFS to the typed function fields the MCP
// server takes. Wrapping rather than passing the FS directly keeps the mcp
// package free of weft-internal types.
func fsLs(s *shell.Shell) mcp.LsFunc {
	return func(path string) ([]string, error) {
		if s.FS == nil {
			return nil, fmt.Errorf("shell has no FS attached")
		}
		entries, err := s.FS.ReadDir(context.Background(), path)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			names = append(names, n)
		}
		return names, nil
	}
}

func fsCat(s *shell.Shell) mcp.CatFunc {
	return func(path string) ([]byte, error) {
		if s.FS == nil {
			return nil, fmt.Errorf("shell has no FS attached")
		}
		return s.FS.ReadFile(context.Background(), path)
	}
}

// wireProposalBackend hooks the propose/apply tools' getter and saver functions
// to the live HTTP backend. The backend's Watch* methods are the storage layer
// for crons ("watch" is the legacy name for the same concept).
func wireProposalBackend(cfg *mcp.Config, b *shell.HTTPBackend) {
	cfg.GetScript = func(name string) (string, error) {
		s, err := b.GetFunction(name)
		if err != nil {
			// "not found" = empty current, which is the correct state for a
			// brand-new resource.
			return "", nil
		}
		return s, nil
	}
	cfg.ListScripts = b.ListFunctions
	cfg.GetCron = func(name string) (string, error) {
		w, err := b.GetWatch(name)
		if err != nil {
			return "", nil
		}
		return fmt.Sprintf("schedule: %s\npipeline: %s\n", w.Schedule, w.Pipeline), nil
	}

	cfg.SaveScript = b.SaveFunction
	cfg.DeleteScript = b.DeleteFunction
	// Cron save: the canonical proposal body is "schedule:/pipeline:" lines;
	// parseCronBody (shared with `propose apply`) splits it back out.
	cfg.SaveCron = func(name, body string) error {
		schedule, pipeline, err := parseCronBody(body)
		if err != nil {
			return err
		}
		return b.SaveWatch(name, &shell.WatchData{Schedule: schedule, Pipeline: pipeline})
	}
	cfg.DeleteCron = b.DeleteWatch

	// The dashboard proposal body is the raw YAML, so save is a pass-through.
	// ListWatches feeds the cron-ref check in dashboard_propose so the model
	// can't stage a dashboard that names crons that don't exist yet.
	cfg.GetDashboard = func(name string) (string, error) {
		body, err := b.GetDashboard(name)
		if err != nil {
			return "", nil
		}
		return body, nil
	}
	cfg.ListWatches = b.ListWatches
	cfg.SaveDashboard = b.SaveDashboard
	cfg.DeleteDashboard = b.DeleteDashboard
}
