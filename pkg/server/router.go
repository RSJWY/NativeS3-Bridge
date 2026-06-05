package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/handlers"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type Middleware func(http.Handler) http.Handler

type Router struct {
	objectHandler *handlers.ObjectHandler
	bucketHandler *handlers.BucketHandler
	chain         []Middleware
}

func NewRouter(backend storage.Backend, authenticator auth.Authenticator, commit handlers.UsageCommitter) http.Handler {
	r := &Router{
		objectHandler: handlers.NewObjectHandler(backend, commit),
		bucketHandler: handlers.NewBucketHandler(backend),
		chain:         []Middleware{Recover, Logging, Auth(authenticator), Quota},
	}
	var h http.Handler = http.HandlerFunc(r.dispatch)
	for i := len(r.chain) - 1; i >= 0; i-- {
		h = r.chain[i](h)
	}
	return h
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
		case http.MethodHead:
			r.bucketHandler.HeadBucket(w, req, bucket)
		case http.MethodGet:
			r.objectHandler.ListObjectsV2(w, req, bucket)
		default:
			handlers.WriteS3Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed, req.URL.Path)
		}
		return
	}

	switch req.Method {
	case http.MethodPut:
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
		next.ServeHTTP(w, r)
		slog.Info("s3 request", "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(started))
	})
}

func Auth(authenticator auth.Authenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := authenticator.Verify(r)
			if err != nil {
				code := auth.ErrorCode(err)
				handlers.WriteS3Error(w, code, http.StatusForbidden, r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	}
}

func Quota(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, key := parseS3Path(r.URL.Path)
		if r.Method == http.MethodPut && bucket != "" && key != "" {
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
