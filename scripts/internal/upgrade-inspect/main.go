package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func main() {
	dbPath := flag.String("db", "", "SQLite database path")
	accessKey := flag.String("access-key", "", "expected credential access key")
	secretKey := flag.String("secret-key", "", "expected credential secret key")
	bucket := flag.String("bucket", "", "expected bucket")
	expectAgent := flag.Bool("expect-agent", false, "expect node-agent additive tables")
	flag.Parse()

	if *dbPath == "" || *accessKey == "" || *secretKey == "" || *bucket == "" {
		fatalf("db, access-key, secret-key and bucket are required")
	}

	db.SetLogLevel("error")
	gdb, err := db.Open("sqlite", *dbPath)
	if err != nil {
		fatalf("open database: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		fatalf("database handle: %v", err)
	}
	defer sqlDB.Close()

	var integrity string
	if err := gdb.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil {
		fatalf("integrity check: %v", err)
	}
	if integrity != "ok" {
		fatalf("integrity check returned %q", integrity)
	}

	for _, table := range []string{"credentials", "request_stats", "hook_configs", "buckets"} {
		assertTable(gdb, table, true)
	}
	for _, index := range []string{"idx_credentials_access_key", "idx_cred_day", "idx_buckets_name"} {
		assertIndex(gdb, index)
	}
	assertTable(gdb, "agent_meta", *expectAgent)
	assertTable(gdb, "applied_tasks", *expectAgent)

	var credential struct {
		SecretKey string
		Status    string
	}
	if err := gdb.Table("credentials").Select("secret_key, status").Where("access_key = ?", *accessKey).Take(&credential).Error; err != nil {
		fatalf("expected credential %q: %v", *accessKey, err)
	}
	if credential.SecretKey != *secretKey || credential.Status != "enabled" {
		fatalf("credential mismatch: secret=%q status=%q", credential.SecretKey, credential.Status)
	}

	var bucketCount int64
	if err := gdb.Table("buckets").Where("name = ?", *bucket).Count(&bucketCount).Error; err != nil {
		fatalf("query bucket %q: %v", *bucket, err)
	}
	if bucketCount != 1 {
		fatalf("bucket %q count = %d, want 1", *bucket, bucketCount)
	}

	fmt.Printf("database inspection passed: %s (agent_tables=%t)\n", *dbPath, *expectAgent)
}

func assertTable(gdb *gorm.DB, name string, expected bool) {
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count).Error; err != nil {
		fatalf("query table %q: %v", name, err)
	}
	if (count == 1) != expected {
		fatalf("table %q presence = %t, want %t", name, count == 1, expected)
	}
}

func assertIndex(gdb *gorm.DB, name string) {
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&count).Error; err != nil {
		fatalf("query index %q: %v", name, err)
	}
	if count != 1 {
		fatalf("index %q count = %d, want 1", name, count)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "upgrade inspection failed: "+format+"\n", args...)
	os.Exit(1)
}
