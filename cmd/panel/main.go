// Command panel is the 国内面板 entry point. It hosts the human admin surface
// (WebAdmin UI + REST) and the node control-plane listener (mTLS WebSocket +
// one-shot registration). It has NO S3 data plane: object traffic never transits
// the panel (design §1.3). The panel refuses to start without its master key and
// intermediate CA (fail-closed, design §7.1).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	loggingpkg "github.com/RSJWY/NativeS3-Bridge/pkg/logging"
	"github.com/RSJWY/NativeS3-Bridge/pkg/panel"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	cfgPath := flag.String("config", "configs/panel.yaml", "panel config file path")
	checkConfig := flag.Bool("check-config", false, "load and validate panel config, then exit")
	flag.Parse()

	cfg, err := config.LoadPanel(*cfgPath)
	if err != nil {
		slog.Error("load panel config", "error", err)
		os.Exit(1)
	}

	if _, err := setupSlog(cfg.LogLevel, cfg.Log); err != nil {
		fmt.Fprintln(os.Stderr, "configure logging:", err)
		os.Exit(1)
	}
	db.SetLogLevel(cfg.LogLevel)

	// Fail-closed dependencies: master key + intermediate CA must load or the panel
	// refuses to start (design §7.1). Load them before opening anything else so a
	// misconfigured panel never half-starts.
	masterKey, err := panel.LoadMasterKeyFromFile(cfg.MasterKeyFile)
	if err != nil {
		slog.Error("load master key", "error", err)
		os.Exit(1)
	}
	cipher, err := panel.NewSecretCipher(masterKey)
	if err != nil {
		slog.Error("init secret cipher", "error", err)
		os.Exit(1)
	}
	ca, err := panel.LoadIntermediateCA(cfg.PKI.IntermediateCertFile, cfg.PKI.IntermediateKeyFile)
	if err != nil {
		slog.Error("load intermediate CA", "error", err)
		os.Exit(1)
	}

	if err := webadmin.BootstrapPasswordHash(&cfg.WebAdmin); err != nil {
		slog.Error("bootstrap webadmin password", "error", err)
		os.Exit(1)
	}

	if *checkConfig {
		slog.Info("panel config check passed")
		return
	}

	gdb, err := db.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	// Panel schema migrates independently of the node DB (physically separate).
	if err := panel.Migrate(gdb); err != nil {
		slog.Error("migrate panel database", "error", err)
		os.Exit(1)
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	hub := panel.NewHub()
	creds := panel.NewPanelCredentialStore(gdb, cipher)
	desired := panel.NewDesiredStateAuthority(gdb, cipher)
	auditor := panel.NewAuditor(gdb)
	tasks := panel.NewTaskOrchestrator(gdb, hub, panel.DefaultTaskTimeout)
	migration := panel.NewMigrationCoordinator(gdb, cipher, desired, auditor)

	transport := panel.NewTransportServer(panel.TransportDeps{
		DB:            gdb,
		CA:            ca,
		Hub:           hub,
		Cipher:        cipher,
		ClientCTTL:    cfg.PKI.ClientCertTTL,
		MigrationSink: migration,
		// On disconnect, mark any in-flight tasks unknown (no silent retry, §5.3).
		OnDisconnected: func(conn *panel.AgentConn) { tasks.FailInFlightForConn(conn) },
	})

	adminServer, err := panel.NewAdminServer(panel.AdminServerDeps{
		Config:    cfg,
		DB:        gdb,
		Hub:       hub,
		Creds:     creds,
		Desired:   desired,
		Tasks:     tasks,
		Transport: transport,
		Migration: migration,
		Audit:     auditor,
	})
	if err != nil {
		slog.Error("init admin server", "error", err)
		os.Exit(1)
	}

	// Node control-plane listener: server TLS + optional client cert (mTLS enforced
	// per-route). The /agent route requires a verified client cert; /register does
	// not (the node has no cert yet).
	serverCert, err := tls.LoadX509KeyPair(cfg.Agent.CertFile, cfg.Agent.KeyFile)
	if err != nil {
		slog.Error("load agent listener cert", "error", err)
		os.Exit(1)
	}
	agentServer := &http.Server{
		Addr:              cfg.Agent.Addr,
		Handler:           transport.Handler(),
		TLSConfig:         transport.ListenerTLSConfig(serverCert),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background offline sweeper: marks nodes offline after missed heartbeats. It
	// only updates observed state; it never touches a node's data plane.
	go runOfflineSweeper(ctx, transport, cfg.HeartbeatInterval, cfg.OfflineMultiplier)

	errCh := make(chan error, 2)
	go func() { errCh <- adminServer.Run(ctx) }()
	go func() { errCh <- runAgentListener(ctx, agentServer) }()

	firstErr := <-errCh
	cancel()
	secondErr := <-errCh
	for _, e := range []error{firstErr, secondErr} {
		if e != nil {
			slog.Error("panel server exited", "error", e)
			os.Exit(1)
		}
	}
}

// runAgentListener serves the node control-plane listener over TLS and shuts it
// down on context cancellation.
func runAgentListener(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting node control-plane listener", "addr", srv.Addr)
		// Cert/key are supplied via TLSConfig.Certificates, so pass empty paths.
		errCh <- srv.ListenAndServeTLS("", "")
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// runOfflineSweeper periodically marks stale nodes offline.
func runOfflineSweeper(ctx context.Context, transport *panel.TransportServer, interval time.Duration, multiplier int) {
	if interval <= 0 {
		interval = panel.DefaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := transport.SweepOffline(interval, multiplier); err != nil {
				slog.Warn("offline sweep failed", "error", err)
			}
		}
	}
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
