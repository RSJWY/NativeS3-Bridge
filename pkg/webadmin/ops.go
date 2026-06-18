package webadmin

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

var errOpsDBUnavailable = errors.New("database unavailable")

// OpsHandler serves health and metrics endpoints for container/observability
// tooling. These are intentionally unauthenticated: liveness/readiness probes
// and Prometheus scrapers must reach them without admin credentials. They
// expose only aggregate counters, no secrets.
type OpsHandler struct {
	db  *gorm.DB
	cfg config.OpsConfig
}

func NewOpsHandler(gdb *gorm.DB, cfgs ...config.OpsConfig) *OpsHandler {
	cfg := config.OpsConfig{PublicHealthz: true, PublicReadyz: true, PublicMetrics: true}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
		if !cfg.PublicHealthz && !cfg.PublicReadyz && !cfg.PublicMetrics && cfg.MetricsToken == "" {
			cfg.PublicHealthz = true
		}
	}
	return &OpsHandler{db: gdb, cfg: cfg}
}

// Healthz is a liveness probe: the process is up and serving.
func (o *OpsHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	if !o.allowPublic(w, r, o.cfg.PublicHealthz) {
		return
	}
	if !requireGet(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz is a readiness probe: dependencies (the database) are reachable.
func (o *OpsHandler) Readyz(w http.ResponseWriter, r *http.Request) {
	if !o.allowPublic(w, r, o.cfg.PublicReadyz) {
		return
	}
	if !requireGet(w, r) {
		return
	}
	if err := o.pingDB(); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// Metrics exposes Prometheus text-format metrics derived from the request_stats
// and credential/bucket tables. Hand-written exposition keeps the dependency
// surface minimal (no client_golang).
func (o *OpsHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	if !o.allowMetrics(w, r) {
		return
	}
	if !requireGet(w, r) {
		return
	}
	var agg struct {
		PutCount    int64
		GetCount    int64
		DeleteCount int64
		BytesIn     int64
		BytesOut    int64
	}
	dbUp := int64(1)
	if o.pingDB() != nil {
		dbUp = 0
	}
	var credentials, buckets int64
	var quotaBytes, usedBytes int64
	if o == nil || o.db == nil {
		dbUp = 0
	} else {
		row := o.db.Model(&dbpkg.RequestStat{}).
			Select("COALESCE(SUM(put_count),0) AS put_count, COALESCE(SUM(get_count),0) AS get_count, COALESCE(SUM(delete_count),0) AS delete_count, COALESCE(SUM(bytes_in),0) AS bytes_in, COALESCE(SUM(bytes_out),0) AS bytes_out").
			Row()
		if err := row.Scan(&agg.PutCount, &agg.GetCount, &agg.DeleteCount, &agg.BytesIn, &agg.BytesOut); err != nil {
			dbUp = 0
		}
		if err := o.db.Model(&dbpkg.Credential{}).Count(&credentials).Error; err != nil {
			dbUp = 0
		}
		if err := o.db.Model(&dbpkg.Bucket{}).Count(&buckets).Error; err != nil {
			dbUp = 0
		}
		if err := o.db.Model(&dbpkg.Credential{}).Select("COALESCE(SUM(quota_bytes),0), COALESCE(SUM(used_bytes),0)").Row().Scan(&quotaBytes, &usedBytes); err != nil {
			dbUp = 0
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	bw := &metricsWriter{w: w}
	bw.counter("natives3_requests_total", "Total S3 object requests by operation", map[string]int64{
		"put":    agg.PutCount,
		"get":    agg.GetCount,
		"delete": agg.DeleteCount,
	}, "op")
	bw.counterValue("natives3_bytes_in_total", "Total bytes written via PUT", agg.BytesIn)
	bw.counterValue("natives3_bytes_out_total", "Total bytes read via GET", agg.BytesOut)
	bw.gauge("natives3_credentials", "Number of credentials", credentials)
	bw.gauge("natives3_buckets", "Number of buckets", buckets)
	bw.gauge("natives3_quota_bytes_total", "Sum of configured quota bytes", quotaBytes)
	bw.gauge("natives3_used_bytes_total", "Sum of used bytes across credentials", usedBytes)
	bw.gauge("natives3_database_up", "1 if the database is reachable, else 0", dbUp)
}

func (o *OpsHandler) allowPublic(w http.ResponseWriter, r *http.Request, allowed bool) bool {
	if allowed {
		return true
	}
	http.NotFound(w, r)
	return false
}

func (o *OpsHandler) allowMetrics(w http.ResponseWriter, r *http.Request) bool {
	if o.cfg.PublicMetrics {
		return true
	}
	if o.cfg.MetricsToken == "" {
		http.NotFound(w, r)
		return false
	}
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if !strings.HasPrefix(value, prefix) || subtle.ConstantTimeCompare([]byte(token), []byte(o.cfg.MetricsToken)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="natives3-metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (o *OpsHandler) pingDB() error {
	if o == nil || o.db == nil {
		return errOpsDBUnavailable
	}
	sqlDB, err := o.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

func requireGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Allow", http.MethodGet)
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte("method not allowed"))
	return false
}

type metricsWriter struct {
	w http.ResponseWriter
}

func (m *metricsWriter) gauge(name, help string, value int64) {
	fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

func (m *metricsWriter) counterValue(name, help string, value int64) {
	fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

func (m *metricsWriter) counter(name, help string, byLabel map[string]int64, label string) {
	fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	// Deterministic label order for stable scrapes.
	for _, key := range []string{"put", "get", "delete"} {
		if v, ok := byLabel[key]; ok {
			fmt.Fprintf(m.w, "%s{%s=%q} %d\n", name, label, key, v)
		}
	}
}
