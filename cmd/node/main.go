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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"gopkg.in/natefinch/lumberjack.v2"
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
	authenticator := auth.NewLocalSigV4Authenticator(credentialStore, cfg.Region)
	hookManager := hooks.NewManager(gdb, hooks.Config{QueueSize: cfg.Hooks.QueueSize, Workers: cfg.Hooks.Workers, MaxRetry: cfg.Hooks.MaxRetry, Timeout: cfg.Hooks.Timeout})
	hookManager.Start()
	defer hookManager.Stop()
	quotaManager := quota.NewManager(gdb)
	// Reuse the monolith's ServerConfig shape for the S3 listener; the node has
	// no admin listener so AdminAddr is left empty.
	s3ServerCfg := config.ServerConfig{S3Addr: cfg.Server.S3Addr, TLS: cfg.Server.TLS}
	s3Server := server.NewManagedWithQuotaManager(s3ServerCfg, backend, multipartStore, bucketStore, authenticator, quotaManager, hookManager, rateLimitController)

	// Control-plane agent: registration (first boot) + mTLS client loop.
	agentDone := startAgent(ctx, cfg, gdb, credentialStore, bucketStore, hookManager, rateLimitController, logRing)

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

func probeS3Listener(cfg *config.NodeConfig) error {
	host, port, err := net.SplitHostPort(cfg.Server.S3Addr)
	if err != nil {
		return fmt.Errorf("parse server.s3_addr: %w", err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	scheme := "http"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Server.TLS.Enabled {
		scheme = "https"
		// This is a loopback-only reachability probe. The serving certificate may
		// be issued for the public S3 hostname rather than the normalized bind IP.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		return fmt.Errorf("invalid HTTP status %d", resp.StatusCode)
	}
	return nil
}

// startAgent registers the node on first boot (if a token is configured and no
// certificate exists yet) and runs the control-plane client loop in the
// background. It returns a channel closed when the agent loop exits. Agent
// failures never stop the S3 data plane (safety net A).
func startAgent(ctx context.Context, cfg *config.NodeConfig, gdb *gorm.DB, invalidator nodeagent.CredentialInvalidator, bucketInvalidator nodeagent.BucketInvalidator, hookReplacer nodeagent.WebhookReplacer, rateLimitUpdater nodeagent.RateLimitUpdater, logRing *loggingpkg.Ring) <-chan struct{} {
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
				slog.Warn("node is not registered and no registration token/url configured; serving S3 from local DB only")
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
		client := nodeagent.NewClient(nodeagent.ClientConfig{
			AgentURL:          cfg.Panel.AgentURL,
			NodeID:            cfg.Panel.NodeID,
			Identity:          identity,
			HeartbeatInterval: cfg.Panel.HeartbeatInterval,
		}, gdb, executor, runner)

		if err := client.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("control-plane agent stopped", "error", err)
		}
	}()
	return done
}

func setupSlog(level string, logCfg config.LogConfig) (*loggingpkg.Ring, error) {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	writers := []io.Writer{os.Stdout}
	logFile := logCfg.EffectiveFile()
	if logFile != "" {
		directory := filepath.Dir(logFile)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create log directory %q: %w", directory, err)
		}
		fileWriter := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    logCfg.MaxSizeMB,
			MaxBackups: logCfg.MaxBackups,
			MaxAge:     logCfg.MaxAgeDays,
			Compress:   logCfg.Compress,
			LocalTime:  true,
		}
		if _, err := fileWriter.Write(nil); err != nil {
			return nil, fmt.Errorf("open log file %q: %w", logFile, err)
		}
		writers = append(writers, fileWriter)
	}
	ring := loggingpkg.NewRing(loggingpkg.DefaultRingCapacity)
	base := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slogLevel})
	slog.SetDefault(slog.New(loggingpkg.NewRingHandler(base, ring)))
	return ring, nil
}
