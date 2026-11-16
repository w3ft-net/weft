package logdb

import "github.com/w3ft-net/weft/backends/shardstore"

// bucketRows is the buckets table as the BucketManager reads and writes it.
type bucketRows interface {
	ListBuckets() ([]shardstore.Bucket, error)
	GetBucket(id int64) (shardstore.Bucket, bool, error)
	GetBucketByLabel(label string) (shardstore.Bucket, bool, error)
	SaveBucket(b *shardstore.Bucket) error
}
