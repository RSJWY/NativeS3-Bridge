package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/handlers"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type Middleware func(http.Handler) http.Handler

type Router struct {
	objectHandler *handlers.ObjectHandler
	bucketHandler *handlers.BucketHandler
	chain         []Middleware
}

func NewRouter(backend storage.Backend) http.Handler {
	r := &Router{
		objectHandler: handlers.NewObjectHandler(backend),
		bucketHandler: handlers.NewBucketHandler(backend),
		chain:         []Middleware{Recover, Logging, AuthNoop, QuotaNoop},
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

func AuthNoop(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func QuotaNoop(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
