package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var configuredLogLevel atomic.Int32

func SetLogLevel(level string) {
	switch strings.ToLower(level) {
	case "debug":
		configuredLogLevel.Store(int32(logger.Info))
	case "error":
		configuredLogLevel.Store(int32(logger.Error))
	case "warn":
		configuredLogLevel.Store(int32(logger.Warn))
	case "info":
		configuredLogLevel.Store(int32(logger.Info))
	default:
		configuredLogLevel.Store(int32(logger.Info))
	}
}

func Open(driver, dsn string) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch driver {
	case "sqlite":
		dialector = sqlite.Open(dsn)
	case "mysql":
		dialector = mysql.Open(dsn)
	case "postgres":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported db driver: %q", driver)
	}

	logLevel := logger.LogLevel(configuredLogLevel.Load())
	if logLevel == 0 {
		logLevel = logger.Info
	}

	return gorm.Open(dialector, &gorm.Config{
		Logger: slogGORMLogger{level: logLevel},
	})
}

type slogGORMLogger struct {
	level logger.LogLevel
}

func (l slogGORMLogger) LogMode(level logger.LogLevel) logger.Interface {
	l.level = level
	return l
}

func (l slogGORMLogger) Info(ctx context.Context, msg string, args ...interface{}) {
	if l.level >= logger.Info {
		slog.InfoContext(ctx, fmt.Sprintf(msg, args...))
	}
}

func (l slogGORMLogger) Warn(ctx context.Context, msg string, args ...interface{}) {
	if l.level >= logger.Warn {
		slog.WarnContext(ctx, fmt.Sprintf(msg, args...))
	}
}

func (l slogGORMLogger) Error(ctx context.Context, msg string, args ...interface{}) {
	if l.level >= logger.Error {
		slog.ErrorContext(ctx, fmt.Sprintf(msg, args...))
	}
}

func (l slogGORMLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level == logger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()
	attrs := []any{"elapsed", elapsed, "rows", rows, "sql", sql}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.level >= logger.Error {
		slog.ErrorContext(ctx, "gorm query failed", append(attrs, "error", err)...)
		return
	}
	if elapsed > 200*time.Millisecond && l.level >= logger.Warn {
		slog.WarnContext(ctx, "gorm slow query", attrs...)
		return
	}
	if l.level >= logger.Info {
		slog.InfoContext(ctx, "gorm query", attrs...)
	}
}
