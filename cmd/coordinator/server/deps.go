// Package server is the coordinator process: flags, listeners, the store, the
// shard control plane, the gRPC servers and their shutdown. It is everything
// that is the same in every deployment, and it names none of the things that
// differ. A deployment's main supplies those as the deployment.Deployment its
// open function returns, which the api reads too, and the Own it adds to it:
//
//	func main() { server.Run(flags, dep, own) }
package server

import (
	"context"
	"errors"
	"fmt"
	weftcfg "github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/ingest"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/coordinator"
	coordsvc "github.com/w3ft-net/weft/coordinator/service"
	"github.com/w3ft-net/weft/deployment"
	"github.com/w3ft-net/weft/logdb"
)

// Own is what only the coordinator asks of a deployment, beside the
// deployment.Deployment every process shares. A field marked optional left nil
// is the open default: no feature, or the one-group behaviour.
type Own struct {
	// ExportStorage is the object store the ephemeral archive export writes its
	// manifests to (ext/storage/s3). Nil: shards archive in process.
	ExportStorage logdb.ExportStorage
	// Classifier builds the LLM-based schema-field classifier (ext/llm/bedrock),
	// or returns nil when it is not configured. Nil: the coordinator has none.
	Classifier func(ctx context.Context) (ingest.Classifier, error)
	// TailBus builds the cross-coordinator tail -f bus for the address given with
	// --redis-addr (ext/tailfanout/redis). Nil: this build links none, and a
	// process given an address refuses to start.
	TailBus func(addr string, tls bool) coordinator.TailBus
	// ConfigKinds are the config sources WEFT_CONFIG_SOURCE can name beyond the
	// file (ext/config/ssm).
	ConfigKinds []weftcfg.Kind
	// Orgs maps a group's org handle to the shape. Required.
	Orgs coordinator.Orgs
	// Backends is where the coordinator's own data lives. Required, except
	// where a member says otherwise.
	Backends Backends
	// ColdScans meters cold-storage reads against the plan's cap. Optional:
	// they are not metered.
	ColdScans *coordinator.ColdScanLedger
	// Commands registers the deployment's own shell commands. Optional.
	Commands coordinator.CommandsFunc
	// Retention is the retention feature for the shard control plane. Optional:
	// archives are kept until an operator removes them.
	Retention logdb.Retention
	// LegacyFactTable names the table of field facts from shards that predate
	// table names. Optional: such facts are unknown.
	LegacyFactTable logdb.LegacyFactTable
	// Extension is the deployment's own behaviour beside the core's. Nil: none.
	Extension Extension
}

// Backends is where the coordinator keeps what the api does not read: the
// shared stores are in the deployment.
type Backends struct {
	// Notices is where user notices live.
	Notices notice.Store
	// Coordination is how this coordinator knows it may act: the leases that
	// elect its leaders and the claims that keep a cron slot from running twice.
	Coordination coordination.Coordination
	// Shards is where shard state lives: shard rows, buckets and heartbeats.
	// Services is the sources registry the provisioner asks which services a
	// bucket serves. Standbys is the warm pool's rows. Optional: none.
	Shards   logdb.ShardStore
	Services logdb.ServiceDirectory
	Standbys logdb.StandbyStore
	// DeadLetters is where a batch the coordinator cannot place on a shard goes,
	// and where the replay loop reads it back from. Optional: such batches are
	// dropped.
	DeadLetters dlq.Store
	// AgentSeen batches the token last-seen stamps the service writes on every
	// stream open, flushed by Run until the process stops. Optional: dropped.
	AgentSeen AgentSeen
}

// AgentSeen records when an agent's token was last seen, without a database
// write per stream open.
type AgentSeen interface {
	Touch(tokenID uint, clientID string, at time.Time)
	Run(ctx context.Context, interval time.Duration)
}

// Extension is the deployment's own behaviour on the running process (hosted:
// the subscription an agent must hold, usage metering, dev seeding). Embed
// NopExtension and override what is needed.
type Extension interface {
	// ConfigureService sets what the gRPC service needs of the deployment: the
	// subscription an agent must hold, the usage it reports, the names of its
	// orgs. Without it the service admits every group and reports none of that.
	ConfigureService(srv *coordsvc.Server)
	// Start runs the deployment's own startup work, once the shard control
	// plane is up and before the crons start.
	Start(rt Runtime)
}

// NopExtension does nothing. Embed it in an Extension to override only some.
type NopExtension struct{}

func (NopExtension) ConfigureService(*coordsvc.Server) {}
func (NopExtension) Start(Runtime)                     {}

// Runtime is what Extension.Start gets of the running process.
type Runtime struct {
	Coord *coordinator.Coordinator
	// Ctx lives as long as the coordinator's drain loop does.
	Ctx context.Context
	// Customer is the --customer flag: a fallback group for dev and docker.
	Customer string

	// Holder identifies this process in the lease table, and Leases is where
	// leases are taken. AWSRegion is the --aws-region flag, and InstanceID the
	// EC2 instance id (empty off EC2).
	Holder     string
	Leases     coordination.Leases
	AWSRegion  string
	InstanceID string
	// AwaitOnExit registers background work whose end the process waits for
	// (for at most a few seconds) when it stops, after Ctx is cancelled: an
	// election that gives its lease back, say. done is closed when it has ended.
	AwaitOnExit func(done <-chan struct{})
}

// validate is why a process whose deployment lacks a required part does not
// start: the coordinator has no default for any of them.
func validate(dep *deployment.Deployment, own *Own) error {
	if err := dep.Validate(); err != nil {
		return err
	}
	if own == nil {
		return errors.New("the deployment supplied no coordinator Own")
	}
	b := own.Backends
	var missing []string
	for _, p := range []struct {
		name string
		set  bool
	}{
		{"Authn.GRPC", dep.Authn.GRPC != nil},
		{"Own.Orgs", own.Orgs != nil},
		{"Own.Backends.Notices", b.Notices != nil},
		{"Own.Backends.Coordination", b.Coordination != nil},
		{"Own.Backends.Shards", b.Shards != nil},
		{"Own.Backends.Services", b.Services != nil},
	} {
		if !p.set {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the deployment lacks %s: the coordinator has no default for them", strings.Join(missing, ", "))
	}
	return nil
}

// CoordinatorConfig fills cfg with what the deployment says about its groups,
// over its backends.
func CoordinatorConfig(dep *deployment.Deployment, own *Own, cfg coordinator.CoordinatorConfig) coordinator.CoordinatorConfig {
	b := dep.Backends
	cfg.Orgs = own.Orgs
	cfg.Scoper = dep.Access.Scope
	cfg.Authz = dep.Access.Authz
	cfg.Entitlements = dep.Entitlements
	cfg.Crons = b.Crons
	cfg.Bins = b.Bins
	cfg.Sources = b.Sources
	cfg.Notices = own.Backends.Notices
	cfg.Shards = own.Backends.Shards
	cfg.Services = own.Backends.Services
	cfg.Errors = coordinator.NewIngestionErrorRecorder(b.IngestFailures)
	cfg.Mounts = dep.Mounts
	cfg.ColdScans = own.ColdScans
	cfg.Commands = own.Commands
	return cfg
}
