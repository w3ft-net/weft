// Package ec2 launches shards as EC2 instances: the instance and its data volume
// (EC2Provisioner, EC2VolumeManager), the warm pool of standby instances, and the
// snapshot and short-lived instance the archive export runs on (ExportResources).
// FromEnv is the logdb.Platform the SHARD_* and EXPORT_* environment describes.
package ec2

import "github.com/w3ft-net/weft/logdb"

var (
	_ logdb.ShardLauncher   = (*EC2Provisioner)(nil)
	_ logdb.ShardLaunchAPI  = (*EC2Provisioner)(nil)
	_ logdb.StandbyLauncher = (*EC2Provisioner)(nil)
	_ logdb.VolumeManager   = (*EC2VolumeManager)(nil)
	_ logdb.ExportOps       = (*ExportResources)(nil)
	_ logdb.ExportLister    = (*ExportResources)(nil)
)
