package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type Server struct {
	httpServer *http.Server
	tls        config.TLSConfig
}

func New(cfg config.ServerConfig, backend storage.Backend) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.S3Addr,
			Handler:           NewRouter(backend),
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls: cfg.TLS,
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
