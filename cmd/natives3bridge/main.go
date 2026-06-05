package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/server"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "config file path")
	seedAccessKey := flag.String("seed-access-key", "", "temporary seed access key for local S3 testing")
	seedSecretKey := flag.String("seed-secret-key", "", "temporary seed secret key for local S3 testing")
	seedQuotaBytes := flag.Int64("seed-quota-bytes", 0, "temporary seed quota bytes; 0 means unlimited")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	setupSlog(cfg.LogLevel)
	db.SetLogLevel(cfg.LogLevel)
	if err := webadmin.BootstrapPasswordHash(&cfg.WebAdmin); err != nil {
		slog.Error("bootstrap webadmin password", "error", err)
		os.Exit(1)
	}

	gdb, err := db.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}

	if err := db.Migrate(gdb); err != nil {
		slog.Error("migrate database", "error", err)
		os.Exit(1)
	}
	if (*seedAccessKey != "" || *seedSecretKey != "") && (*seedAccessKey == "" || *seedSecretKey == "") {
		slog.Error("seed access key and secret key must be provided together")
		os.Exit(1)
	}
	if *seedAccessKey != "" {
		if err := seedCredential(gdb, *seedAccessKey, *seedSecretKey, *seedQuotaBytes); err != nil {
			slog.Error("seed credential", "error", err)
			os.Exit(1)
		}
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
	s3Server := server.New(cfg.Server, backend, multipartStore, authenticator, func(credID uint, deltaBytes int64, op quota.Op) error {
		return quota.Commit(gdb, credID, deltaBytes, op)
	}, hookManager)
	adminServer, err := webadmin.NewServer(cfg.Server, cfg.WebAdmin, gdb, credentialStore)
	if err != nil {
		slog.Error("init admin server", "error", err)
		os.Exit(1)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- s3Server.Run(ctx) }()
	go func() { errCh <- adminServer.Run(ctx) }()

	firstErr := <-errCh
	cancel()
	secondErr := <-errCh
	if firstErr != nil {
		slog.Error("run server", "error", firstErr)
		os.Exit(1)
	}
	if secondErr != nil {
		slog.Error("run server", "error", secondErr)
		os.Exit(1)
	}
}

func seedCredential(gdb *gorm.DB, accessKey, secretKey string, quotaBytes int64) error {
	cred := db.Credential{AccessKey: accessKey, SecretKey: secretKey, Name: "local seed", Status: "enabled", QuotaBytes: quotaBytes}
	return gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "access_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"secret_key":  secretKey,
			"status":      "enabled",
			"quota_bytes": quotaBytes,
		}),
	}).Create(&cred).Error
}

func setupSlog(level string) {
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

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel})
	slog.SetDefault(slog.New(handler))
}
