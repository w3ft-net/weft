package server

import (
	"context"
	"flag"
	"github.com/w3ft-net/weft/transport"
	"log"
	"net/http"
	"os"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/alerting"
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/cluster"
	"github.com/w3ft-net/weft/cmd/api/server/options"
	"github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/deployment"
	ingsyslog "github.com/w3ft-net/weft/ingest/syslog"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/sources"
	"github.com/w3ft-net/weft/web"
)

// Flags are the api's command-line flags, parsed.
type Flags = options.Flags

// ParseFlags starts logging, then defines and parses the api's flags, over the
// cluster file if there is one (--config, or $WEFT_CONFIG). An empty
// --db-dsn is filled from DB_DSN, so Flags.DBDSN is the DSN to open (empty: the
// deployment's own default). Call it before opening anything that logs.
func ParseFlags() *Flags {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	fl := options.Define(flag.CommandLine)
	cfgPath := cluster.DefineFlag(flag.CommandLine)
	flag.Parse()
	if err := cluster.Overlay(flag.CommandLine, cluster.API, *cfgPath); err != nil {
		log.Fatalf("api: %v", err)
	}
	if fl.DBDSN == "" {
		fl.DBDSN = os.Getenv("DB_DSN")
	}
	return fl
}

// Run runs the HTTP api process over the deployment deps describes, which the
// caller has opened (and will close when Run returns). It returns only if the
// server stops; a start that cannot proceed exits the process, as cmd/api
// always has.
func Run(fl *Flags, dep *deployment.Deployment, own *Own) {
	port, coordAddr, baseURL := &fl.Port, &fl.CoordinatorAddr, &fl.BaseURL
	renderSyslogPort := &fl.RenderSyslogPort

	if err := fl.TLS.Apply(); err != nil {
		log.Fatalf("api: %v", err)
	}
	if err := validateURLs(fl); err != nil {
		log.Fatalf("api: %v", err)
	}
	if err := validate(dep); err != nil {
		log.Fatalf("api: %v", err)
	}
	planner.SetDialects(dep.Dialects)
	if own == nil {
		own = &Own{}
	}

	srv := weftapi.NewServer(*coordAddr, *baseURL)
	srv.SetAPIBaseURL(fl.APIBaseURL)
	srv.SetIdentity(dep.Authn.API)
	if app := web.Handler(); app != nil {
		// This binary carries the web app (-tags webembed): serve it at the root,
		// beside the api routes, which answer first.
		srv.Extend(func(rt *weftapi.Routes) { rt.Public("/", app) })
	}
	if err := srv.Connect(); err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer srv.Close()

	srv.SetAccess(dep.Access)
	if own.Groups != nil {
		srv.SetGroups(own.Groups)
	}
	if dep.Entitlements != nil {
		srv.SetEntitlements(dep.Entitlements)
	}
	b := dep.Backends
	srv.SetTokens(b.Tokens)
	srv.SetSources(b.Sources)
	srv.SetCrons(b.Crons)
	srv.SetBins(b.Bins)
	if b.Dashboards != nil {
		srv.SetDashboards(b.Dashboards)
	}
	srv.SetNotify(b.Notify)
	srv.SetIngestFailures(b.IngestFailures)
	srv.SetMounts(dep.Mounts)
	if own.AgentFleet != nil {
		srv.SetAgentFleet(own.AgentFleet)
	}
	ext := own.Extension
	if ext == nil {
		ext = NopExtension{}
	}
	if dep.AlertTargets != nil {
		srv.SetAlertKinds(alerting.NewRegistry(srv.NotifyService(), dep.AlertTargets...))
	}

	// LLM-based schema-field classification for pending sources — see
	// ingest/llmdetect.go. Off unless the deployment supplies a classifier and
	// it is configured; that is an opt-in, not a startup requirement, since a
	// model has to be provisioned separately.
	if own.Classifier != nil {
		if classifier, err := own.Classifier(context.Background()); err != nil {
			weftsdk.Warnf(context.Background(), "WARNING: schema classifier disabled: %v", err)
		} else if classifier != nil {
			srv.SetClassifier(classifier)
		}
	}

	// The deployment's own configuration of the server (hosted: Stripe prices
	// and keys, Slack client).
	ext.Configure(srv)
	// The producers the deployment adds, beside the core's. Render's TLS syslog
	// listener is one: it binds the port its producer is given.
	_ = srv.Producers().Register("render", ingsyslog.RenderProducer{ListenPort: *renderSyslogPort})
	ext.Producers(srv.Producers())
	srv.Extend(ext.Routes)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The deployment's own background work (hosted: the periodic cleanup of
	// accounts that never completed email verification).
	ext.Start(ctx, srv)

	// TLS syslog listeners, one per syslog-speaking platform.
	// weft-nlb terminates TLS against the platform's own
	// hostname and ACM cert and forwards plaintext TCP here; the port IS the
	// producer identity, so this process never needs to sniff a connection to
	// know it's Render. A second platform is a producer registered under its own
	// name with its own port, not new code here.
	sources.Each(srv.Producers(), func(name string, p ingsyslog.StreamProducer) {
		ss := srv.NewSyslogServer(p)
		if err := ss.Start(ctx); err != nil {
			log.Fatalf("start %s syslog listener: %v", name, err)
		}
	})

	// The deployment's own work that runs beside the listeners (hosted: the
	// daily Stripe usage reporter).
	ext.Serving(ctx, srv)

	// Kick off the runtime-config watcher: the YAML file (or SSM, with
	// WEFT_CONFIG_SOURCE=ssm) overridden by the environment, re-read every 60s
	// and on SIGHUP, atomically swapping the pointer every rate-limit, body-cap
	// and grpc-cap consumer holds. A source that cannot be read falls through to
	// compile-time defaults. AWS_REGION, when set, is where SSM is.
	region := os.Getenv("AWS_REGION")
	srv.SetConfig(config.Watch(ctx, config.SourceFromEnv(region, own.ConfigKinds...), 60*time.Second))

	weftsdk.Infof(ctx, "weft-api listening on :%s (coordinator: %s)", *port, *coordAddr)
	var err error
	httpSrv := &http.Server{Addr: ":" + *port, Handler: srv.Handler(), TLSConfig: transport.ServerTLS()}
	if httpSrv.TLSConfig != nil {
		err = httpSrv.ListenAndServeTLS("", "")
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil {
		log.Fatal(err)
	}
}
