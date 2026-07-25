package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Runtime contains the process-local logging resources installed by Setup.
// Ring is always non-nil and receives the same records that are sent to the
// configured slog handler. File is the effective active file path, or an empty
// string when file logging is disabled.
//
// The lumberjack writer is intentionally owned by the process logger for its
// lifetime. Commands normally exit with the process, so no explicit close is
// required; tests can rely on the active file being created before Setup
// returns.
type Runtime struct {
	Ring *Ring
	File string
}

// Setup installs the shared stdout + optional lumberjack + in-memory ring slog
// pipeline. All service entry points use this helper so level parsing,
// effective-file handling, and rotation options cannot drift between Panel,
// Node, and the legacy standalone command.
func Setup(level string, logCfg config.LogConfig) (*Runtime, error) {
	writers := []io.Writer{os.Stdout}
	logFile := logCfg.EffectiveFile()
	if logFile != "" {
		directory := filepath.Dir(logFile)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create log directory %q: %w", directory, err)
		}
		// Validate/create the active file without asking lumberjack to write.
		// Even an empty lumberjack write can rotate an already-oversized file,
		// which makes merely starting or checking configuration mutate history.
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open log file %q: %w", logFile, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close log file %q: %w", logFile, err)
		}
		fileWriter := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    logCfg.MaxSizeMB,
			MaxBackups: logCfg.MaxBackups,
			MaxAge:     logCfg.MaxAgeDays,
			Compress:   logCfg.Compress,
			LocalTime:  true,
		}
		writers = append(writers, fileWriter)
	}

	ring := NewRing(DefaultRingCapacity)
	base := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: ParseLevel(level)})
	slog.SetDefault(slog.New(NewRingHandler(base, ring)))
	return &Runtime{Ring: ring, File: logFile}, nil
}

// ParseLevel normalizes the command/config log level using the shared policy.
// Unknown and empty values retain the historical INFO behavior.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
