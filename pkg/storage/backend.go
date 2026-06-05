package storage

import (
	"io"
	"time"
)

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string
}

type BucketInfo struct {
	Name         string
	CreationDate time.Time
}

type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
}

type Range struct {
	Start int64
	End   int64
}

type Backend interface {
	PutObject(bucket, key string, r io.Reader, contentType string) (ObjectInfo, error)
	GetObject(bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)
	HeadObject(bucket, key string) (ObjectInfo, error)
	DeleteObject(bucket, key string) error
	ListObjects(bucket, prefix, delimiter, token string, maxKeys int) (ListResult, error)
	ListBuckets() ([]BucketInfo, error)
}
