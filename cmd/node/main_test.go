package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
)

func TestSetupSlogUsesSharedFileAndRingContract(t *testing.T) {
	originalLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	path := filepath.Join(t.TempDir(), "logs", "node.log")
	ring, err := setupSlog("info", config.LogConfig{File: path, MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("node setup test", "node_id", 7)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if entries := ring.Snapshot(1, "INFO", "node_id"); len(entries) != 1 || entries[0].Message != "node setup test" {
		t.Fatalf("ring entries = %+v", entries)
	}
}

func TestProbeS3ListenerAcceptsS3ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.NodeConfig{Server: config.NodeServerConfig{S3Addr: net.JoinHostPort("0.0.0.0", port)}}
	if err := probeS3Listener(cfg); err != nil {
		t.Fatalf("probe running S3 listener: %v", err)
	}
}

func TestProbeS3ListenerRejectsPlainHTTPService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 普通 HTTP 服务返回 200 HTML 目录页,不是 S3 XML 错误。
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.NodeConfig{Server: config.NodeServerConfig{S3Addr: net.JoinHostPort("0.0.0.0", port)}}
	if err := probeS3Listener(cfg); err == nil {
		t.Fatal("probe unexpectedly passed for plain HTTP service")
	}
}

func TestProbeS3ListenerFailsWhenPortIsClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	cfg := &config.NodeConfig{Server: config.NodeServerConfig{S3Addr: addr}}
	if err := probeS3Listener(cfg); err == nil {
		t.Fatal("probe unexpectedly passed for closed port")
	}
}
