package cli

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shell"
)

type env struct {
	opts    Options
	orgFlag string
}

func (e *env) ClientID() string                 { return e.opts.ClientID }
func (e *env) OrgFlag() *string                 { return &e.orgFlag }
func (e *env) Config() (*shell.Config, error)   { return shell.LoadConfig() }
func (e *env) SaveConfig(c *shell.Config) error { return shell.SaveConfig(c) }

func (e *env) Org(cfg *shell.Config) string {
	if e.orgFlag != "" {
		return e.orgFlag
	}
	return cfg.Org
}

func (e *env) Credential(cfg *shell.Config) (string, error) { return e.opts.Auth.Credential(cfg) }

// session is the loaded config with its endpoint checked and its credential.
func (e *env) session() (cfg *shell.Config, credential string, err error) {
	cfg, err = e.Config()
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		return nil, "", fmt.Errorf("endpoint %q is not an api URL: set it to http(s)://<api-host> in ~/.weft.yaml", cfg.Endpoint)
	}
	credential, err = e.Credential(cfg)
	if err != nil {
		return nil, "", err
	}
	return cfg, credential, nil
}

func (e *env) Coordinator() (*shell.HTTPCoordinator, error) {
	cfg, cred, err := e.session()
	if err != nil {
		return nil, err
	}
	c := shell.NewHTTPCoordinator(cfg.Endpoint, cred, e.Org(cfg))
	c.SetClientID(e.opts.ClientID)
	return c, nil
}

func (e *env) Backend() (*shell.HTTPBackend, error) {
	cfg, cred, err := e.session()
	if err != nil {
		return nil, err
	}
	return shell.NewHTTPBackend(cfg.Endpoint, cred, e.Org(cfg)), nil
}

// Shell builds a shell backed by the api over HTTP. The CLI only speaks HTTP:
// direct-to-coordinator gRPC was retired because it doubled the public auth
// surface (the api already gates credentials, rate limits, CORS and the rest).
func (e *env) Shell() (*shell.Shell, error) {
	cfg, cred, err := e.session()
	if err != nil {
		return nil, err
	}
	org := e.Org(cfg)
	remote := shell.NewHTTPCoordinator(cfg.Endpoint, cred, org)
	remote.SetClientID(e.opts.ClientID)
	apps, err := remote.ListApps()
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	s := &shell.Shell{
		Tiers: e.opts.Tiers,
		Query: func(pipeline, rangeSpec string, dirStack []string) (*shell.QueryResult, error) {
			r, err := remote.Query(pipeline, rangeSpec, dirStack)
			if err != nil {
				return nil, err
			}
			notices := make([]shell.QueryResultNotice, len(r.Notices))
			for i, n := range r.Notices {
				notices[i] = shell.QueryResultNotice{Key: n.Key, Kind: n.Kind, Title: n.Title, Body: n.Body}
			}
			rows := make([]shell.Row, len(r.Rows))
			for i, row := range r.Rows {
				rows[i] = shell.Row{Raw: row.Raw}
			}
			return &shell.QueryResult{Rows: rows, Text: r.Text, Notices: notices, FinalDirStack: r.FinalDirStack}, nil
		},
		Follow: func(ctx context.Context, pipeline, rangeSpec string, onRow func(shell.Row)) error {
			return remote.Follow(ctx, pipeline, rangeSpec, func(row *pb.ResultRow) {
				onRow(shell.Row{Raw: row.Raw})
			})
		},
		Range: "1d..",
		Apps:  apps,
		FS:    shell.NewProxyFS(cfg.Endpoint, cred, org),
	}
	return s, nil
}
