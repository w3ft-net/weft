package compute

import "os"

// ResolveRegion returns the AWS region the compute worker uses for
// Lambda cold-shard SigV4 signing.
//
// AWS SDK v2's default config chain does not reliably derive a region
// from IMDS (commit f931874 fixed the identical failure in
// coordinator/shard/api). When fan-out + Lambda signing moved into the
// compute worker (d477ea2) this resolution was skipped, so on prod
// compute instances — where weft-compute.service ships no
// AWS_REGION — signing used an empty region and every cold-shard
// query got a 403 "Credential should be scoped to a valid region".
//
// AWS_REGION is set by EC2 user-data / the service EnvironmentFile;
// fall back to us-east-2 to match cmd/shard and cmd/api.
func ResolveRegion() string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	return "us-east-2"
}
