package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	loggingpkg "github.com/RSJWY/NativeS3-Bridge/pkg/logging"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/server"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"github.com/RSJWY/NativeS3-Bridge/pkg/webadmin"
	"gopkg.in/natefinch/lumberjack.v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "config file path")
	checkConfig := flag.Bool("check-config", false, "load and validate config, then print production hardening warnings")
	seedAccessKey := flag.String("seed-access-key", "", "temporary seed access key for local S3 testing")
	seedSecretKey := flag.String("seed-secret-key", "", "temporary seed secret key for local S3 testing")
	seedQuotaBytes := flag.Int64("seed-quota-bytes", 0, "temporary seed quota bytes; 0 means unlimited")
	seedBucket := flag.String("seed-bucket", "", "scope seed credential to a single bucket; empty means all buckets")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logRing, err := setupSlog(cfg.LogLevel, cfg.Log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure logging:", err)
		os.Exit(1)
	}
	db.SetLogLevel(cfg.LogLevel)
	logProductionWarnings(cfg)
	if *checkConfig {
		slog.Info("config check passed")
		return
	}
	if err := webadmin.BootstrapPasswordHash(&cfg.WebAdmin); err != nil {
		slog.Error("bootstrap webadmin password", "error", err)
		os.Exit(1)
	}

	gdb, err := db.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}

	if err := db.MigrateConfigured(cfg.Database.Driver, cfg.Database.DSN, gdb); err != nil {
		slog.Error("migrate database", "error", err)
		os.Exit(1)
	}
	if (*seedAccessKey != "" || *seedSecretKey != "") && (*seedAccessKey == "" || *seedSecretKey == "") {
		slog.Error("seed access key and secret key must be provided together")
		os.Exit(1)
	}
	if *seedAccessKey != "" {
		if err := seedCredential(gdb, *seedAccessKey, *seedSecretKey, *seedQuotaBytes, *seedBucket); err != nil {
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
	multipartStore.SetMaxPendingBytes(cfg.Storage.MultipartMaxPendingBytes)
	bucketStore := storage.NewBucketStore(gdb, cfg.Storage.DataRoot, storage.DefaultBucketACLCacheTTL)

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
	boundCredentialChecker := func(bucket string) (bool, error) {
		var count int64
		err := gdb.Model(&db.Credential{}).Where("bucket = ?", bucket).Count(&count).Error
		return count > 0, err
	}
	s3Server := server.NewWithQuotaManager(cfg.Server, cfg.RateLimit, backend, multipartStore, bucketStore, authenticator, quotaManager, boundCredentialChecker, hookManager)
	adminServer, err := webadmin.NewServer(cfg.Server, cfg.WebAdmin, gdb, credentialStore, bucketStore, webadmin.ServerOptions{
		TrustForwarded: cfg.RateLimit.TrustForwarded,
		LogRing:        logRing,
		LogFile:        cfg.Log.EffectiveFile(),
		DataRoot:       cfg.Storage.DataRoot,
		MetadataSuffix: cfg.Storage.MetadataSuffix,
	})
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

func seedCredential(gdb *gorm.DB, accessKey, secretKey string, quotaBytes int64, bucket string) error {
	if bucket != "" {
		var count int64
		if err := gdb.Model(&db.Bucket{}).Where("name = ?", bucket).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("seed bucket %q does not exist", bucket)
		}
	}
	cred := db.Credential{AccessKey: accessKey, SecretKey: secretKey, Name: "local seed", Bucket: bucket, Status: "enabled", QuotaBytes: quotaBytes}
	return gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "access_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"secret_key":  secretKey,
			"status":      "enabled",
			"quota_bytes": quotaBytes,
			"bucket":      bucket,
		}),
	}).Create(&cred).Error
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

func logProductionWarnings(cfg *config.Config) {
	for _, warning := range cfg.ProductionWarnings() {
		slog.Warn("production config warning", "check", warning)
	}
}
