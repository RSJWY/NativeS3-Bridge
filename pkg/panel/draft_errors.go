package panel

import "errors"

var (
	ErrNodeBucketNotFound  = errors.New("node bucket not found")
	ErrNodeBucketExists    = errors.New("node bucket already exists")
	ErrNodeBucketBound     = errors.New("node bucket has bound credentials")
	ErrNodeWebhookNotFound = errors.New("node webhook not found")
)
