package panel

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin/ui"
)

// AdminServer hosts the panel's human-facing management surface: the embedded
// WebAdmin SPA plus the panel REST API, both behind the reused webadmin auth
// stack (login/session/lockout/TOTP/captcha). It listens on the admin address,
// entirely separate from the node control-plane listener.
type AdminServer struct {
	httpServer *http.Server
	tls        config.TLSConfig
}

// AdminServerDeps are the collaborators the admin server needs.
type AdminServerDeps struct {
	Config    *config.PanelConfig
	DB        *gorm.DB
	Hub       *Hub
	Creds     *CredentialStore
	Desired   *DesiredStateAuthority
	Tasks     *TaskOrchestrator
	Transport *TransportServer
	Migration *MigrationCoordinator
	Audit     *Auditor
}

// NewAdminServer builds the admin HTTP server. It wires the reused webadmin auth
// middleware in front of the panel REST API and serves the embedded SPA for all
// non-API routes.
func NewAdminServer(deps AdminServerDeps) (*AdminServer, error) {
	webCfg := deps.Config.WebAdmin
	effectiveTLS := deps.Config.EffectiveAdminTLS()
	authenticator := webadmin.NewAuth(webCfg, effectiveTLS.Enabled)

	mux := http.NewServeMux()
	adminAPI := NewAdminAPI(deps.DB, deps.Hub, deps.Creds, deps.Desired, deps.Tasks, deps.Transport, deps.Migration, deps.Audit)

	// Auth endpoints (login/logout/settings) reuse the webadmin handlers.
	mux.HandleFunc("/api/admin/auth-settings", authenticator.AuthSettings)
	mux.HandleFunc("/api/admin/login", authenticator.Login)
	mux.Handle("/api/admin/logout", authenticator.Middleware(http.HandlerFunc(authenticator.Logout)))

	// Panel REST API behind auth middleware.
	adminAPI.Routes(mux, authenticator.Middleware)

	// Everything else serves the embedded SPA.
	staticFS, err := fs.Sub(ui.DistFS, "dist")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", spaHandler(staticFS))

	return &AdminServer{
		httpServer: &http.Server{
			Addr:              deps.Config.AdminAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		tls: effectiveTLS,
	}, nil
}

// Run starts the admin server and shuts it down on context cancellation.
func (s *AdminServer) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting panel admin server", "addr", s.httpServer.Addr)
		if s.tls.Enabled {
			errCh <- s.httpServer.ListenAndServeTLS(s.tls.CertFile, s.tls.KeyFile)
			return
		}
		slog.Warn("panel admin UI served over plain HTTP; put it behind trusted HTTPS")
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

// spaHandler serves the embedded SPA, falling back to index.html for client-side
// routes. Mirrors the webadmin SPA handler (kept package-local; the webadmin
// version is unexported).
func spaHandler(staticFS fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeTransportError(w, http.StatusNotFound, "not found")
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
