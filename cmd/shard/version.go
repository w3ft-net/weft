package main

// GitSHA is the build commit's SHA. The Packer build for the shard tier
// sets it via `-ldflags "-X main.GitSHA=$GIT_SHA"`; locally-built binaries
// leave it empty. The shard reports it on every heartbeat so the
// coordinator persists it on shard_records.binary_sha, and weft-admin uses
// it to skip rolling shards already on the target binary.
var GitSHA = ""
