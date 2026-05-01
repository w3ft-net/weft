package main

import (
	"context"
	"fmt"
	"io"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kevlarcon/weft/cmd/weft/mcp"
	"github.com/kevlarcon/weft/cmd/weft/mcp/proposals"
	"github.com/kevlarcon/weft/shell"
)

// runMCP starts the weft MCP server on stdio. It reuses the same
// auth/connection path as `weft sh`: load ~/.weft.yaml, connect to
// the api server with the user's JWT, and surface the resulting
// Shell + RemoteFS to the SDK as the seam underneath weft.run /
// weft.ls / weft.cat.
//
// Stdio is the right transport for the local-LLM use case (Claude
// Desktop, Cursor, Claude Code all spawn MCP servers as
// subprocesses and talk newline-delimited JSON over stdin/stdout).
// Anything we print to stderr is invisible to the protocol but
// shows up in the host's MCP server logs, which is useful for
// debugging.
func runMCP() {
	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
	}

	s, cleanup, err := connectRemote(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	mcpCfg := mcp.Config{
		Run: func(pipeline string, w io.Writer) error {
			return s.ExecNonInteractive(pipeline, w)
		},
		Ls:  fsLs(s),
		Cat: fsCat(s),
	}

	// Wire propose/apply against the same on-disk staging dir
	// `weft propose review/apply/clear` reads. If the user can't
	// reach a writable dir (read-only HOME, exotic env) we skip
	// propose tools rather than failing the whole MCP startup.
	if dir, err := defaultProposalsDir(); err == nil {
		if store, err := proposals.New(dir); err == nil {
			mcpCfg.Proposals = store
			backend := shell.NewHTTPBackend(cfg.Endpoint, cfg.Token)
			wireProposalBackend(&mcpCfg, backend)
		} else {
			fmt.Fprintf(os.Stderr, "warning: propose tools disabled — could not open %s: %v\n", dir, err)
		}
	}

	server := mcp.NewServer(mcpCfg)

	// Connect to stdio. server.Run blocks until the client closes
	// the connection or sends a shutdown.
	ctx := context.Background()
	if err := server.Run(ctx, &sdkmcp.StdioTransport{}); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "mcp server: %v\n", err)
		os.Exit(1)
	}
}

// fsLs and fsCat adapt the shell's RemoteFS to the typed function
// fields the MCP server takes. Wrapping rather than passing the FS
// directly keeps the mcp package free of weft-internal types.
func fsLs(s *shell.Shell) mcp.LsFunc {
	return func(path string) ([]string, error) {
		if s.FS == nil {
			return nil, fmt.Errorf("shell has no FS attached")
		}
		return s.FS.ReadDir(path)
	}
}

func fsCat(s *shell.Shell) mcp.CatFunc {
	return func(path string) ([]byte, error) {
		if s.FS == nil {
			return nil, fmt.Errorf("shell has no FS attached")
		}
		return s.FS.ReadFile(path)
	}
}

// wireProposalBackend hooks the propose/apply tools' getter and
// saver functions to the live HTTP backend. The backend's Watch*
// methods are the storage layer for crons — "watch" is the legacy
// name for the same concept, scheduled for renaming separately.
func wireProposalBackend(cfg *mcp.Config, b *shell.HTTPBackend) {
	cfg.GetScript = func(name string) (string, error) {
		s, err := b.GetFunction(name)
		if err != nil {
			// "not found" = empty current, which is the correct
			// state for a brand-new resource.
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
	// Cron save: the canonical proposal body shape is
	// "schedule:/pipeline:" lines; parseCronBody (shared with the
	// CLI) splits it back out for the underlying SaveWatch.
	cfg.SaveCron = func(name, body string) error {
		schedule, pipeline, err := parseCronBody(body)
		if err != nil {
			return err
		}
		return b.SaveWatch(name, &shell.WatchData{
			Schedule: schedule,
			Pipeline: pipeline,
		})
	}
	cfg.DeleteCron = b.DeleteWatch

	// Dashboard wiring: the proposal body is the raw YAML, so save
	// is a thin pass-through. ListWatches feeds the watch-ref check
	// in dashboard_propose so the model can't stage a dashboard
	// referencing watches that don't exist yet.
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
