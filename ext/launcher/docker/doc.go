// Package docker launches shards as sibling Docker containers through the Docker
// socket: one container per shard, each with a bind-mounted data directory. It is
// the launcher of a single machine and of local development; production uses a
// cloud launcher (ext/launcher/ec2).
package docker

import "github.com/w3ft-net/weft/logdb"

var _ logdb.ShardLauncher = (*Launcher)(nil)
