package shard

import "github.com/w3ft-net/weft/backends/logs/archive/parquet"

// The archive layout code lives in backends/logs/archive, where a store that is
// not DuckDB can use it. These are the names this package has always exported,
// which deployments build on.
type (
	ExportManifest = parquet.ExportManifest
	ManifestStore  = parquet.ManifestStore
	ManifestReader = parquet.ManifestReader
)

var (
	PartitionMinutes   = parquet.PartitionMinutes
	IsKeyed            = parquet.IsKeyed
	IsSized            = parquet.IsSized
	BucketStart        = parquet.BucketStart
	SizedArchivePath   = parquet.SizedArchivePath
	ExportOutputSuffix = parquet.ExportOutputSuffix
	ExportOutputPaths  = parquet.ExportOutputPaths
	ManifestKey        = parquet.ManifestKey
)
