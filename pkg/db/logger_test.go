package db

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm/logger"
)

func TestGORMLoggerRedactsSQLStringLiterals(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	SetLogLevel("info")
	gdb, err := Open("sqlite", t.TempDir()+"/natives3.db")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	const secret = `do-not-log-"secret"-value`
	if err := gdb.Create(&Credential{AccessKey: "LOGAK", SecretKey: secret, Name: "log test", Status: "enabled"}).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}

	logs := buf.String()
	for _, leaked := range []string{secret, "LOGAK", "log test"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("gorm logs leaked SQL string literal %q:\n%s", leaked, logs)
		}
	}
	for _, want := range []string{"elapsed=", "rows=", "sql=", "[redacted]"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("gorm logs missing %q:\n%s", want, logs)
		}
	}
}

func TestGORMLoggerRedactsSlowAndErrorQueries(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	log := slogGORMLogger{level: logger.Warn}
	begin := time.Now().Add(-250 * time.Millisecond)
	log.Trace(context.Background(), begin, func() (string, int64) {
		return `UPDATE credentials SET secret_key="slow-secret" WHERE access_key="LOGAK"`, 1
	}, nil)

	log = slogGORMLogger{level: logger.Error}
	log.Trace(context.Background(), time.Now(), func() (string, int64) {
		return `INSERT INTO credentials (access_key, secret_key) VALUES ('ERRAK', 'error-secret')`, 0
	}, errors.New("boom"))

	logs := buf.String()
	for _, leaked := range []string{"slow-secret", "LOGAK", "ERRAK", "error-secret"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("gorm logs leaked SQL string literal %q:\n%s", leaked, logs)
		}
	}
	if !strings.Contains(logs, "gorm slow query") || !strings.Contains(logs, "gorm query failed") {
		t.Fatalf("gorm logs missing slow/error messages:\n%s", logs)
	}
}

func TestRedactSQLLiteralsHandlesEscapedQuotes(t *testing.T) {
	sql := `INSERT INTO credentials (access_key, secret_key, name) VALUES ('A''K', "S\"K", "plain")`
	redacted := redactSQLLiterals(sql)
	for _, leaked := range []string{"A''K", `S\"K`, "plain"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("redacted SQL leaked %q: %s", leaked, redacted)
		}
	}
	if got, want := strings.Count(redacted, "[redacted]"), 3; got != want {
		t.Fatalf("redacted literal count = %d, want %d: %s", got, want, redacted)
	}
}
