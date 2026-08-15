package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/handlers"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type Server struct {
	httpServer *http.Server
	tls        config.TLSConfig
	// router 仅 managed 构造路径保留,用于在启动前注入节点级遥测计数器。
	router *Router
}

func New(cfg config.ServerConfig, rateLimit config.RateLimitConfig, backend storage.Backend, multipartStore *storage.MultipartStore, bucketStore *storage.BucketStore, authenticator auth.Authenticator, commit handlers.UsageCommitter, emitter handlers.EventEmitter) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.S3Addr,
			Handler:           NewRouter(backend, multipartStore, bucketStore, authenticator, commit, emitter, rateLimit),
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls: cfg.TLS,
	}
}

func NewWithQuotaManager(cfg config.ServerConfig, rateLimit config.RateLimitConfig, backend storage.Backend, multipartStore *storage.MultipartStore, bucketStore *storage.BucketStore, authenticator auth.Authenticator, manager handlers.QuotaManager, boundCredentialChecker func(string) (bool, error), emitter handlers.EventEmitter) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.S3Addr,
			Handler:           NewRouterWithQuotaManager(backend, multipartStore, bucketStore, authenticator, manager, boundCredentialChecker, emitter, rateLimit),
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls: cfg.TLS,
	}
}

func NewManagedWithQuotaManager(cfg config.ServerConfig, backend storage.Backend, multipartStore *storage.MultipartStore, bucketStore *storage.BucketStore, authenticator auth.Authenticator, manager handlers.QuotaManager, emitter handlers.EventEmitter, rateLimit *RateLimitController) *Server {
	router, handler := NewManagedRouterWithQuotaManagerParts(backend, multipartStore, bucketStore, authenticator, manager, emitter, rateLimit)
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.S3Addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls:    cfg.TLS,
		router: router,
	}
}

// SetTelemetryRecorder 注入节点级存储遥测计数器(managed 节点专用)。必须在
// Run 开始接受流量之前调用;standalone 构造路径不注入,行为不变。
func (s *Server) SetTelemetryRecorder(recorder handlers.TelemetryRecorder) {
	if s.router != nil {
		s.router.SetTelemetryRecorder(recorder)
	}
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting s3 server", "addr", s.httpServer.Addr)
		if s.tls.Enabled {
			errCh <- s.httpServer.ListenAndServeTLS(s.tls.CertFile, s.tls.KeyFile)
			return
		}
		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
