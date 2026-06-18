package webadmin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	s3auth "github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

func TestOpsHealthzAndReadyz(t *testing.T) {
	gdb := newWebadminTestDB(t)
	ops := NewOpsHandler(gdb)

	rr := httptest.NewRecorder()
	ops.Healthz(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("healthz content type = %q, want text/plain; charset=utf-8", got)
	}
	if got := rr.Body.String(); got != "ok" {
		t.Fatalf("healthz body = %q, want ok", got)
	}

	rr = httptest.NewRecorder()
	ops.Readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "ready" {
		t.Fatalf("readyz body = %q, want ready", got)
	}
}

func TestOpsRejectsUnsupportedMethods(t *testing.T) {
	ops := NewOpsHandler(newWebadminTestDB(t))
	for _, tt := range []struct {
		name string
		path string
		call func(http.ResponseWriter, *http.Request)
	}{
		{name: "healthz", path: "/healthz", call: ops.Healthz},
		{name: "readyz", path: "/readyz", call: ops.Readyz},
		{name: "metrics", path: "/metrics", call: ops.Metrics},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tt.call(rr, httptest.NewRequest(http.MethodPost, tt.path, nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rr.Code)
			}
			if got := rr.Header().Get("Allow"); got != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", got)
			}
		})
	}
}

func TestOpsReadyzFailsWhenDBUnavailable(t *testing.T) {
	ops := NewOpsHandler(nil)
	rr := httptest.NewRecorder()
	ops.Readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when DB unavailable", rr.Code)
	}
	if got := rr.Body.String(); got != "database unavailable" {
		t.Fatalf("readyz body = %q, want database unavailable", got)
	}
}

func TestOpsReadyzFailsWhenDBClosed(t *testing.T) {
	gdb := newWebadminTestDB(t)
	ops := NewOpsHandler(gdb)
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	_ = sqlDB.Close()

	rr := httptest.NewRecorder()
	ops.Readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when DB down", rr.Code)
	}
}

func TestOpsMetricsReportsDatabaseDownWhenDBClosed(t *testing.T) {
	gdb := newWebadminTestDB(t)
	ops := NewOpsHandler(gdb)
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	_ = sqlDB.Close()

	rr := httptest.NewRecorder()
	ops.Metrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "natives3_database_up 0") {
		t.Fatalf("metrics body missing database down gauge\n---\n%s", rr.Body.String())
	}
}

func TestOpsMetricsReportsDatabaseDownWhenDBUnavailable(t *testing.T) {
	ops := NewOpsHandler(nil)
	rr := httptest.NewRecorder()
	ops.Metrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "natives3_database_up 0") {
		t.Fatalf("metrics body missing database down gauge\n---\n%s", rr.Body.String())
	}
}

func TestOpsMetricsExposesPrometheus(t *testing.T) {
	gdb := newWebadminTestDB(t)
	// Seed a credential and request stats so non-zero metrics appear.
	if err := gdb.Create(&dbpkg.Credential{AccessKey: "ak", SecretKey: "sk", Status: "enabled", QuotaBytes: 100, UsedBytes: 40}).Error; err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if err := gdb.Create(&dbpkg.RequestStat{CredentialID: 1, Day: "2026-06-07", PutCount: 3, GetCount: 5, BytesIn: 30, BytesOut: 50}).Error; err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	if err := gdb.Create(&dbpkg.Bucket{Name: "metrics-bucket", ACL: storage.ACLPrivate}).Error; err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	ops := NewOpsHandler(gdb)

	rr := httptest.NewRecorder()
	ops.Metrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics content type = %q, want Prometheus text content type", got)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`natives3_requests_total{op="put"} 3`,
		`natives3_requests_total{op="get"} 5`,
		`natives3_requests_total{op="delete"} 0`,
		"natives3_bytes_in_total 30",
		"natives3_bytes_out_total 50",
		"natives3_quota_bytes_total 100",
		"natives3_used_bytes_total 40",
		"natives3_credentials 1",
		"natives3_buckets 1",
		"natives3_database_up 1",
		"# TYPE natives3_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q\n---\n%s", want, body)
		}
	}
}

func TestNewServerBlocksReadyzAndMetricsByDefaultAndProtectsAdminAPI(t *testing.T) {
	gdb := newWebadminTestDB(t)
	webCfg := config.WebAdminConfig{PasswordHash: mustPasswordHash(t), SessionSecret: "test-session-secret", SessionTTLMinutes: 10}
	serverCfg := config.ServerConfig{AdminAddr: "127.0.0.1:0"}
	credentialStore := s3auth.NewCredentialStore(gdb, time.Second)
	bucketStore := storage.NewBucketStore(gdb, t.TempDir(), time.Second)
	srv, err := NewServer(serverCfg, webCfg, gdb, credentialStore, bucketStore)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	rr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated metrics status = %d, want 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated readyz status = %d, want 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("unauthenticated healthz status = %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/admin/credentials", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin API status = %d, want 401", rr.Code)
	}
	assertJSONError(t, rr.Body.Bytes(), "unauthorized")
}

func TestNewServerAllowsConfiguredOpsEndpoints(t *testing.T) {
	gdb := newWebadminTestDB(t)
	webCfg := config.WebAdminConfig{
		PasswordHash:      mustPasswordHash(t),
		SessionSecret:     "test-session-secret",
		SessionTTLMinutes: 10,
		Ops: config.OpsConfig{
			PublicHealthz: true,
			PublicReadyz:  true,
			PublicMetrics: true,
		},
	}
	serverCfg := config.ServerConfig{AdminAddr: "127.0.0.1:0"}
	credentialStore := s3auth.NewCredentialStore(gdb, time.Second)
	bucketStore := storage.NewBucketStore(gdb, t.TempDir(), time.Second)
	srv, err := NewServer(serverCfg, webCfg, gdb, credentialStore, bucketStore)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	for _, path := range []string{"/readyz", "/metrics"} {
		rr := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestMetricsTokenAllowsPrivateScrape(t *testing.T) {
	gdb := newWebadminTestDB(t)
	ops := NewOpsHandler(gdb, config.OpsConfig{PublicHealthz: true, MetricsToken: "test-token"})

	rr := httptest.NewRecorder()
	ops.Metrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rr.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	ops.Metrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
