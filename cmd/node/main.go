// Command node is the海外节点 entry point. It runs the S3 data plane and the
// control-plane agent client, and it does NOT start any WebAdmin / management
// listener (design §1.3). The S3 data plane starts from the node-local DB
// regardless of panel connectivity (safety net A): an un-registered or
// disconnected node keeps serving S3 from its last-applied local config while
// the agent attempts to register/reconnect in the background.
package main

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	loggingpkg "github.com/RSJWY/NativeS3-Bridge/pkg/logging"
	"github.com/RSJWY/NativeS3-Bridge/pkg/nodeagent"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/server"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

func main() {
	cfgPath := flag.String("config", "configs/node.yaml", "node config file path")
	checkConfig := flag.Bool("check-config", false, "load and validate node config, then exit")
	health := flag.Bool("health", false, "probe the configured S3 listener, then exit")
	flag.Parse()

	cfg, err := config.LoadNode(*cfgPath)
	if err != nil {
		slog.Error("load node config", "error", err)
		os.Exit(1)
	}

	logRing, err := setupSlog(cfg.LogLevel, cfg.Log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure logging:", err)
		os.Exit(1)
	}
	db.SetLogLevel(cfg.LogLevel)
	// 配置类告警统一放在日志配置完成之后,否则会落到默认 stderr 而不是配置的日志目的地。
	// --check-config 也要打:它正是升级前的自查入口。
	if mode := config.InsecureNodeConfigMode(*cfgPath); mode != 0 {
		slog.Warn("node config file permissions are too permissive",
			"path", *cfgPath,
			"mode", fmt.Sprintf("%04o", mode),
			"hint", "this file holds the registration token and database DSN; run chmod 0600")
	}
	if cfg.Panel.AllowInsecureTransport {
		slog.Warn("panel.allow_insecure_transport is enabled: cleartext control-plane URLs are accepted and mTLS is NOT in effect",
			"agent_url", cfg.Panel.AgentURL,
			"hint", "use wss:// / https:// outside same-host loopback tests")
	}
	if *checkConfig {
		slog.Info("node config check passed")
		return
	}
	if *health {
		if err := probeS3Listener(cfg); err != nil {
			slog.Error("node health probe failed", "error", err)
			os.Exit(1)
		}
		slog.Info("node health probe passed")
		return
	}

	gdb, err := db.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	// Base node schema (credentials/buckets/request_stats/hooks) — unchanged and
	// shared with the pre-multinode binary.
	if err := db.MigrateConfigured(cfg.Database.Driver, cfg.Database.DSN, gdb); err != nil {
		slog.Error("migrate database", "error", err)
		os.Exit(1)
	}
	// Additive agent state tables (safety net C): only added here in cmd/node, so
	// the base schema and the deprecated standalone binary are untouched.
	if err := nodeagent.MigrateState(gdb); err != nil {
		slog.Error("migrate agent state", "error", err)
		os.Exit(1)
	}

	backend, err := storage.NewFileBackendWithMetadataSuffix(cfg.Storage.DataRoot, cfg.Storage.MetadataSuffix)
	if err != nil {
		slog.Error("init storage backend", "error", err)
		os.Exit(1)
	}
	multipartStore, err := storage.NewMultipartStore(cfg.Storage.DataRoot, cfg.Storage.MultipartTmp, cfg.Storage.MetadataSuffix)
	if err != nil {
		slog.Error("init multipart store", "error", err)
		os.Exit(1)
	}
	multipartStore.SetMaxPendingBytes(cfg.Storage.MultipartMaxPendingBytes)
	bucketStore := storage.NewBucketStore(gdb, cfg.Storage.DataRoot, storage.DefaultBucketACLCacheTTL)
	managedRateLimit, _, err := nodeagent.LoadManagedRateLimit(gdb)
	if err != nil {
		slog.Error("load managed rate limit", "error", err)
		os.Exit(1)
	}
	rateLimitController := server.NewRateLimitController(managedRateLimit)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	multipartStore.StartGC(ctx.Done(), cfg.Storage.MultipartGCInterval, cfg.Storage.MultipartTTL)

	credentialStore := auth.NewCredentialStore(gdb, auth.DefaultCredentialCacheTTL)
	v4Authenticator := auth.NewLocalSigV4Authenticator(credentialStore, cfg.Region)
	var v2Authenticator auth.Authenticator
	if cfg.Auth.AllowSigV2 {
		v2Authenticator = auth.NewLocalSigV2Authenticator(credentialStore)
	}
	authenticator := auth.NewMultiSchemeAuthenticator(v4Authenticator, v2Authenticator)
	hookManager := hooks.NewManager(gdb, hooks.Config{QueueSize: cfg.Hooks.QueueSize, Workers: cfg.Hooks.Workers, MaxRetry: cfg.Hooks.MaxRetry, Timeout: cfg.Hooks.Timeout})
	hookManager.Start()
	defer hookManager.Stop()
	quotaManager := quota.NewManager(gdb)
	// Reuse the monolith's ServerConfig shape for the S3 listener; the node has
	// no admin listener so AdminAddr is left empty.
	s3ServerCfg := config.ServerConfig{S3Addr: cfg.Server.S3Addr, TLS: cfg.Server.TLS}
	s3Server := server.NewManagedWithQuotaManager(s3ServerCfg, backend, multipartStore, bucketStore, authenticator, quotaManager, hookManager, rateLimitController)
	// 节点级存储遥测:计数器注入 S3 处理器,基线扫描在开始接受流量之前同步
	// 完成一次(已有 native 文件获得真实初值,且不与在线写入产生扫描竞态)。
	// 基线失败不阻塞 S3 服务:遥测保持"不可用",等待显式 reconcile 重建。
	telemetryRecorder := nodeagent.NewStorageTelemetryRecorder(gdb, cfg.Storage.DataRoot)
	s3Server.SetTelemetryRecorder(telemetryRecorder)
	if err := telemetryRecorder.EnsureStorageTelemetryBaseline(cfg.Storage.DataRoot, cfg.Storage.MetadataSuffix); err != nil {
		slog.Error("storage telemetry baseline failed; telemetry stays unavailable until rebuild", "error", err)
	}

	// Control-plane agent: registration (first boot) + mTLS client loop.
	agentDone := startAgent(ctx, cfg, gdb, credentialStore, bucketStore, hookManager, rateLimitController, logRing, telemetryRecorder)

	errCh := make(chan error, 1)
	go func() { errCh <- s3Server.Run(ctx) }()

	// The S3 data plane is authoritative for liveness: if it exits, we shut down.
	if err := <-errCh; err != nil {
		slog.Error("run s3 server", "error", err)
		cancel()
		<-agentDone
		os.Exit(1)
	}
	cancel()
	<-agentDone
}

// s3ErrorResponse 是网关返回的 S3 XML 错误结构,用于 health 探针区分
// "我们的 S3 网关"与"占用端口的其他 HTTP 服务"。
type s3ErrorResponse struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
}

func probeS3Listener(cfg *config.NodeConfig) error {
	host, port, err := net.SplitHostPort(cfg.Server.S3Addr)
	if err != nil {
		return fmt.Errorf("parse server.s3_addr: %w", err)
	}
	// net.SplitHostPort 返回的 host 不含方括号,"[::]" 死分支删除。
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	scheme := "http"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Server.TLS.Enabled {
		scheme = "https"
		// R7.3:仅当探测目标是 loopback 且证书对探测地址不可验时才允许跳过验证。
		// 公网地址必须可验,避免把任意 HTTPS 服务误判为健康。
		if isLoopback(host) {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec
		} else {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return fmt.Errorf("read probe response: %w", err)
	}

	// 我们的 S3 网关对无签名的 GET / 返回 403 AccessDenied 的 XML 错误。
	// 普通 http.server 会返回 HTML 目录页或 200,不会命中该结构。
	if resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("probe returned status %d, expected 403 AccessDenied", resp.StatusCode)
	}
	var s3Err s3ErrorResponse
	if err := xml.Unmarshal(body, &s3Err); err != nil {
		return fmt.Errorf("probe response is not S3 XML error: %w", err)
	}
	if s3Err.XMLName.Local != "Error" || s3Err.Code == "" {
		return fmt.Errorf("probe response missing S3 Error/Code")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startAgent registers the node on first boot (if a token is configured and no
// certificate exists yet) and runs the control-plane client loop in the
// background. It returns a channel closed when the agent loop exits. Agent
// failures never stop the S3 data plane (safety net A).
func startAgent(ctx context.Context, cfg *config.NodeConfig, gdb *gorm.DB, invalidator nodeagent.CredentialInvalidator, bucketInvalidator nodeagent.BucketInvalidator, hookReplacer nodeagent.WebhookReplacer, rateLimitUpdater nodeagent.RateLimitUpdater, logRing *loggingpkg.Ring, telemetryRecorder *nodeagent.StorageTelemetryRecorder) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)

		identity := nodeagent.Identity{
			KeyFile:  cfg.Panel.KeyFile,
			CertFile: cfg.Panel.CertFile,
			CAFile:   cfg.Panel.CAFile,
		}

		// First-boot registration: only when we have no certificate yet and a
		// token is configured. A registration failure is logged but does NOT stop
		// S3 service; the node keeps serving from local DB and retries later.
		if !identity.HasCertificate() {
			if strings.TrimSpace(cfg.Panel.Token) == "" || strings.TrimSpace(cfg.Panel.RegisterURL) == "" {
				if _, err := os.Stat(identity.CertFile); err == nil {
					slog.Error("local client certificate is expired or damaged; " +
						"request a new registration token from the panel admin, " +
						"set it in node.yaml panel.registration_token, then restart the node")
				} else {
					slog.Error("node is not registered and no registration token/url configured; " +
						"request a registration token from the panel admin, " +
						"set it in node.yaml panel.registration_token, then restart the node")
				}
				return
			}
			slog.Info("node registration starting; transient failures will be retried")
			if err := nodeagent.RegisterWithRetry(ctx, identity, nodeagent.RegisterParams{
				RegisterURL: cfg.Panel.RegisterURL,
				NodeID:      cfg.Panel.NodeID,
				Token:       cfg.Panel.Token,
			}, nodeagent.RegisterRetryOptions{}); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("node registration failed; continuing to serve S3 from local DB", "error", err)
				return
			}
			slog.Info("node registration succeeded")
		}

		executor := nodeagent.NewManagedExecutor(gdb, nodeagent.ExecutorRuntime{
			CredentialInvalidator: invalidator,
			BucketInvalidator:     bucketInvalidator,
			WebhookReplacer:       hookReplacer,
			RateLimitUpdater:      rateLimitUpdater,
			DataRoot:              cfg.Storage.DataRoot,
		})
		runner := nodeagent.NewLocalTaskRunner(gdb, logRing, cfg.Storage.DataRoot, cfg.Storage.MetadataSuffix, invalidator)
		runner.SetTelemetryRecorder(telemetryRecorder)
		client := nodeagent.NewClient(nodeagent.ClientConfig{
			AgentURL:          cfg.Panel.AgentURL,
			NodeID:            cfg.Panel.NodeID,
			Identity:          identity,
			Region:            cfg.Region,
			HeartbeatInterval: cfg.Panel.HeartbeatInterval,
		}, gdb, executor, runner)
		client.SetTelemetryRecorder(telemetryRecorder)

		if err := client.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("control-plane agent stopped", "error", err)
		}
	}()
	return done
}

func setupSlog(level string, logCfg config.LogConfig) (*loggingpkg.Ring, error) {
	runtime, err := loggingpkg.Setup(level, logCfg)
	if err != nil {
		return nil, err
	}
	return runtime.Ring, nil
}
