package logging

import (
	"bytes"
	"path/filepath"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestLumberjackRotatesAndLimitsBackups(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	writer := &lumberjack.Logger{Filename: path, MaxSize: 1, MaxBackups: 1, LocalTime: true}
	defer writer.Close()
	payload := bytes.Repeat([]byte("x"), 700*1024)
	for index := 0; index < 4; index++ {
		if _, err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	backups, err := filepath.Glob(filepath.Join(directory, "app-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) == 0 || len(backups) > 1 {
		t.Fatalf("backup count = %d, want 1", len(backups))
	}
}
