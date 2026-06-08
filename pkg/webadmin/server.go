package webadmin

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	s3auth "github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin/ui"
	"gorm.io/gorm"
)

type Server struct {
	httpServer *http.Server
	tls        config.TLSConfig
}

func NewServer(serverCfg config.ServerConfig, webCfg config.WebAdminConfig, gdb *gorm.DB, credentialStore *s3auth.CredentialStore, bucketStore *storage.BucketStore, trustForwarded ...bool) (*Server, error) {
	effectiveTLS := serverCfg.EffectiveAdminTLS()
	authenticator := NewAuth(webCfg, effectiveTLS.Enabled)
	if len(trustForwarded) > 0 {
		authenticator.trustForwarded = trustForwarded[0]
	}
	api := NewAPI(gdb, credentialStore, bucketStore)
	staticFS, err := fs.Sub(ui.DistFS, "dist")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	ops := NewOpsHandler(gdb)
	mux.HandleFunc("/healthz", ops.Healthz)
	mux.HandleFunc("/readyz", ops.Readyz)
	mux.HandleFunc("/metrics", ops.Metrics)
	mux.HandleFunc("/api/admin/login", authenticator.Login)
	mux.Handle("/api/admin/logout", authenticator.Middleware(http.HandlerFunc(authenticator.Logout)))
	mux.Handle("/api/admin/credentials", authenticator.Middleware(http.HandlerFunc(api.Credentials)))
	mux.Handle("/api/admin/credentials/", authenticator.Middleware(http.HandlerFunc(api.CredentialByID)))
	mux.Handle("/api/admin/buckets", authenticator.Middleware(http.HandlerFunc(api.Buckets)))
	mux.Handle("/api/admin/buckets/", authenticator.Middleware(http.HandlerFunc(api.BucketByName)))
	mux.Handle("/api/admin/dashboard/summary", authenticator.Middleware(http.HandlerFunc(api.DashboardSummary)))
	mux.Handle("/api/admin/dashboard/usage-ranking", authenticator.Middleware(http.HandlerFunc(api.UsageRanking)))
	mux.Handle("/api/admin/dashboard/request-trend", authenticator.Middleware(http.HandlerFunc(api.RequestTrend)))
	mux.Handle("/api/admin", authenticator.Middleware(http.HandlerFunc(adminNotFound)))
	mux.Handle("/api/admin/", authenticator.Middleware(http.HandlerFunc(adminNotFound)))
	mux.Handle("/", spaHandler(staticFS))

	return &Server{
		httpServer: &http.Server{
			Addr:              serverCfg.AdminAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls: effectiveTLS,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting admin server", "addr", s.httpServer.Addr)
		if s.tls.Enabled {
			errCh <- s.httpServer.ListenAndServeTLS(s.tls.CertFile, s.tls.KeyFile)
			return
		}
		slog.Warn("admin UI served over plain HTTP; enable TLS for production")
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

func adminNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not found")
}

func spaHandler(staticFS fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		filePath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if filePath == "" || filePath == "." {
			filePath = "index.html"
		}
		serveEmbeddedFile(w, r, staticFS, filePath)
	})
}

func serveEmbeddedFile(w http.ResponseWriter, r *http.Request, staticFS fs.FS, filePath string) {
	file, err := staticFS.Open(filePath)
	if err != nil {
		serveIndex(w, r, staticFS)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		serveIndex(w, r, staticFS)
		return
	}
	reader, ok := file.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		serveIndex(w, r, staticFS)
		return
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), reader)
}

func serveIndex(w http.ResponseWriter, r *http.Request, staticFS fs.FS) {
	file, err := staticFS.Open("index.html")
	if err != nil {
		http.Error(w, "admin UI is not built", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		http.Error(w, "admin UI is not built", http.StatusServiceUnavailable)
		return
	}
	reader, ok := file.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "admin UI is not built", http.StatusServiceUnavailable)
		return
	}
	http.ServeContent(w, r, "index.html", stat.ModTime(), reader)
}
