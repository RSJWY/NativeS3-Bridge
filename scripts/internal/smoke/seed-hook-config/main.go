package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

func main() {
	driver := flag.String("driver", "sqlite", "database driver")
	dsn := flag.String("dsn", "", "database DSN")
	url := flag.String("url", "", "webhook URL")
	events := flag.String("events", "ObjectCreated,ObjectDeleted", "comma-separated hook events")
	enabled := flag.Bool("enabled", true, "whether the hook is enabled")
	flag.Parse()

	if *dsn == "" || *url == "" {
		fmt.Fprintln(os.Stderr, "-dsn and -url are required")
		os.Exit(2)
	}

	db.SetLogLevel("error")
	gdb, err := db.Open(*driver, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(gdb); err != nil {
		fmt.Fprintf(os.Stderr, "migrate database: %v\n", err)
		os.Exit(1)
	}
	if err := gdb.Create(&db.HookConfig{URL: *url, Events: *events, Enabled: *enabled}).Error; err != nil {
		fmt.Fprintf(os.Stderr, "create hook config: %v\n", err)
		os.Exit(1)
	}
}
