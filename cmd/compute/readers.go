package main

import (
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/shard"
	"github.com/w3ft-net/weft/shard/duckbind"
)

// archiveReaderSet is the readers this process can read archives with: the
// Lambda one, and the in-process DuckDB one in a build that links DuckDB. This is
// the composition; it moves out of core with the storage it names
// (ext/storage/lambda).
func archiveReaderSet(creds aws.CredentialsProvider, region string) map[string]archive.Reader {
	set := map[string]archive.Reader{compute.ArchiveReaderLambda: shard.LambdaReader(creds, region)}
	if duckbind.Linked() {
		set[compute.ArchiveReaderLocal] = shard.LocalReader(shard.S3ConfigFromEnv())
	}
	return set
}
