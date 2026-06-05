# Decisions

## Module Path

- `github.com/RSJWY/NativeS3-Bridge`
- Reason: matches the configured `origin` remote URL for this repository.

## Go Version

- `go 1.21`
- Reason: task requires Go version >= 1.21 and the implementation only uses Go 1.21-compatible language/library features.

## SQLite Driver

- `github.com/glebarez/sqlite`
- Reason: pure Go SQLite driver, matching the task preference to avoid CGO and preserve single-file cross-platform binary behavior.

## Dependency Versions

- `github.com/glebarez/sqlite`: `v1.11.0`
- `gorm.io/gorm`: `v1.31.1`
- `gorm.io/driver/mysql`: `v1.6.0`
- `gorm.io/driver/postgres`: `v1.6.0`
- `gopkg.in/yaml.v3`: `v3.0.1`
- `github.com/rogpeppe/go-internal`: `v1.12.0` (indirect pin to keep `go mod tidy -go=1.21` compatible)
