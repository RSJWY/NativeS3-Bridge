package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/handlers"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type Middleware func(http.Handler) http.Handler

type ACLLookup func(bucket string) (acl string, exists bool, err error)

var s3RequestIDCounter uint64

type Router struct {
	objectHandler    *handlers.ObjectHandler
	bucketHandler    *handlers.BucketHandler
	multipartHandler *handlers.MultipartHandler
	bucketStore      *storage.BucketStore
	chain            []Middleware
}

func NewRouter(backend storage.Backend, multipartStore *storage.MultipartStore, bucketStore *storage.BucketStore, authenticator auth.Authenticator, commit handlers.UsageCommitter, emitter handlers.EventEmitter, rateLimit config.RateLimitConfig) http.Handler {
	r := &Router{
		objectHandler:    handlers.NewObjectHandlerWithHooks(backend, commit, emitter),
		bucketHandler:    handlers.NewBucketHandler(backend, bucketStore),
		multipartHandler: handlers.NewMultipartHandlerWithHooks(multipartStore, commit, emitter),
		bucketStore:      bucketStore,
		chain:            []Middleware{Recover, Logging, AnonRateLimit(rateLimit), Auth(authenticator, bucketStore.GetACL), Quota},
	}
	var h http.Handler = http.HandlerFunc(r.dispatch)
	for i := len(r.chain) - 1; i >= 0; i-- {
		h = r.chain[i](h)
	}
	return h
}

func AnonRateLimit(cfg config.RateLimitConfig) Middleware {
	limiter := newIPRateLimiter(cfg.AnonymousRPS, cfg.AnonymousBurst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bucket, key := parseS3Path(r.URL.Path)
			if hasCredentials(r) || !isAnonymousObjectRead(r, bucket, key) {
				next.ServeHTTP(w, r)
				return
			}
			if !limiter.allow(clientIP(r, cfg.TrustForwarded)) {
				handlers.WriteS3Error(w, "SlowDown", http.StatusServiceUnavailable, r.URL.Path)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (r *Router) dispatch(w http.ResponseWriter, req *http.Request) {
	bucket, key := parseS3Path(req.URL.Path)
	if bucket == "" {
		if req.Method == http.MethodGet {
			r.bucketHandler.ListBuckets(w, req)
			return
		}
		handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		return
	}

	if key == "" {
		switch req.Method {
		case http.MethodPut:
			r.bucketHandler.CreateBucket(w, req, bucket)
		case http.MethodDelete:
			r.bucketHandler.DeleteBucket(w, req, bucket)
		case http.MethodHead:
			r.bucketHandler.HeadBucket(w, req, bucket)
		case http.MethodPost:
			if hasQuery(req, "delete") {
				r.objectHandler.DeleteObjects(w, req, bucket)
				return
			}
			handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		case http.MethodGet:
			if hasQuery(req, "location") {
				r.bucketHandler.GetBucketLocation(w, req, bucket)
				return
			}
			if hasQuery(req, "versioning") {
				r.bucketHandler.GetBucketVersioning(w, req, bucket)
				return
			}
			if hasQuery(req, "uploads") {
				r.multipartHandler.ListUploads(w, req, bucket)
				return
			}
			r.objectHandler.ListObjectsV2(w, req, bucket)
		default:
			handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		}
		return
	}

	if r.shouldEnsureBucketMetadata(req, bucket, key) {
		if err := r.bucketStore.Create(bucket); err != nil {
			writeBucketMetadataError(w, err, req.URL.Path)
			return
		}
	}

	if hasQuery(req, "tagging") {
		switch req.Method {
		case http.MethodPut:
			r.objectHandler.PutTagging(w, req, bucket, key)
		case http.MethodGet:
			r.objectHandler.GetTagging(w, req, bucket, key)
		case http.MethodDelete:
			r.objectHandler.DeleteTagging(w, req, bucket, key)
		default:
			handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		}
		return
	}

	if hasQuery(req, "uploads") && req.Method == http.MethodPost {
		r.multipartHandler.Create(w, req, bucket, key)
		return
	}

	if req.URL.Query().Get("uploadId") != "" {
		switch req.Method {
		case http.MethodPut:
			r.multipartHandler.UploadPart(w, req, bucket, key)
		case http.MethodPost:
			r.multipartHandler.Complete(w, req, bucket, key)
		case http.MethodDelete:
			r.multipartHandler.Abort(w, req, bucket, key)
		case http.MethodGet:
			r.multipartHandler.ListParts(w, req, bucket, key)
		default:
			handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		}
		return
	}

	switch req.Method {
	case http.MethodPut:
		if handlers.IsCopyRequest(req) {
			r.objectHandler.Copy(w, req, bucket, key)
			return
		}
		r.objectHandler.Put(w, req, bucket, key)
	case http.MethodGet:
		r.objectHandler.Get(w, req, bucket, key)
	case http.MethodHead:
		r.objectHandler.Head(w, req, bucket, key)
	case http.MethodDelete:
		r.objectHandler.Delete(w, req, bucket, key)
	default:
		handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
	}
}

func (r *Router) shouldEnsureBucketMetadata(req *http.Request, bucket, key string) bool {
	if r.bucketStore == nil || bucket == "" || key == "" {
		return false
	}
	if req.Method == http.MethodPut && !hasQuery(req, "tagging") && req.URL.Query().Get("uploadId") == "" {
		return true
	}
	return req.Method == http.MethodPost && hasQuery(req, "uploads")
}

func hasQuery(r *http.Request, key string) bool {
	_, ok := r.URL.Query()[key]
	return ok
}

func writeBucketMetadataError(w http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, storage.ErrInvalidBucketName):
		handlers.WriteS3Error(w, "InvalidBucketName", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrInvalidPath):
		handlers.WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, resource)
	default:
		handlers.WriteS3Error(w, "InternalError", http.StatusInternalServerError, resource)
	}
}

func parseS3Path(rawPath string) (string, string) {
	trimmed := strings.TrimPrefix(rawPath, "/")
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("panic in request", "panic", recovered)
				handlers.WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newS3RequestID(started)
		w.Header().Set("x-amz-request-id", requestID)
		defer func() {
			slog.Info("s3 request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(started))
		}()
		next.ServeHTTP(w, r)
	})
}

func newS3RequestID(t time.Time) string {
	seq := atomic.AddUint64(&s3RequestIDCounter, 1)
	return fmt.Sprintf("req-%016x-%08x", t.UTC().UnixNano(), seq&0xffffffff)
}

func Auth(authenticator auth.Authenticator, aclLookup ACLLookup) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hasCredentials(r) {
				id, err := authenticator.Verify(r)
				if err != nil {
					code := auth.ErrorCode(err)
					handlers.WriteS3Error(w, code, http.StatusForbidden, r.URL.Path)
					return
				}
				next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
				return
			}

			bucket, key := parseS3Path(r.URL.Path)
			if !isAnonymousObjectRead(r, bucket, key) || aclLookup == nil {
				handlers.WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
				return
			}
			acl, exists, err := aclLookup(bucket)
			if err != nil {
				slog.Error("lookup bucket acl", "bucket", bucket, "error", err)
				handlers.WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
				return
			}
			if !exists || acl != storage.ACLPublicRead {
				handlers.WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.AnonymousIdentity())))
		})
	}
}

func hasCredentials(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || auth.HasPresignQuery(r)
}

func isAnonymousObjectRead(r *http.Request, bucket, key string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if bucket == "" || key == "" {
		return false
	}
	return !hasAnonymousBlockedSubresource(r)
}

func hasAnonymousBlockedSubresource(r *http.Request) bool {
	query := r.URL.Query()
	for _, key := range []string{"tagging", "uploads", "uploadId", "acl", "tags"} {
		if _, ok := query[key]; ok {
			return true
		}
	}
	return false
}

func Quota(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, key := parseS3Path(r.URL.Path)
		if r.Method == http.MethodPut && bucket != "" && key != "" && !hasQuery(r, "tagging") && r.URL.Query().Get("uploadId") == "" && r.Header.Get("x-amz-copy-source") == "" {
			id, ok := auth.IdentityFromContext(r.Context())
			if !ok || id == nil {
				handlers.WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
				return
			}
			size := contentLengthForQuota(r)
			if size < 0 {
				handlers.WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
				return
			}
			if err := quota.Check(id, size); err != nil {
				if errors.Is(err, quota.ErrQuotaExceeded) {
					handlers.WriteS3Error(w, "QuotaExceeded", http.StatusForbidden, r.URL.Path)
					return
				}
				handlers.WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func contentLengthForQuota(r *http.Request) int64 {
	if raw := r.Header.Get("x-amz-decoded-content-length"); raw != "" {
		size, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return -1
		}
		return size
	}
	return r.ContentLength
}
