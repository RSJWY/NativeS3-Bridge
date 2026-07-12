package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const MaxOrphanSidecarSamples = 50

type ReconcileReport struct {
	Bucket          string
	ObjectCount     int64
	ScannedBytes    int64
	OrphanSidecars  []string
	orphanFullPaths []string
}

func ReconcileBucket(root, bucket, metadataSuffix string) (ReconcileReport, error) {
	bucketPath, err := ResolveBucketPath(root, bucket)
	if err != nil {
		return ReconcileReport{}, err
	}
	stat, err := os.Stat(bucketPath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !stat.IsDir()) {
		return ReconcileReport{}, ErrNoSuchBucket
	}
	if err != nil {
		return ReconcileReport{}, err
	}
	if metadataSuffix == "" {
		metadataSuffix = DefaultMetadataSuffix
	}
	report := ReconcileReport{Bucket: bucket}
	err = filepath.WalkDir(bucketPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".multipart" && path != bucketPath {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, metadataSuffix) {
			objectPath := strings.TrimSuffix(path, metadataSuffix)
			objectStat, objectErr := os.Stat(objectPath)
			if objectErr == nil && objectStat.Mode().IsRegular() {
				return nil
			}
			if objectErr != nil && !errors.Is(objectErr, os.ErrNotExist) {
				return objectErr
			}
			relative, relErr := filepath.Rel(bucketPath, path)
			if relErr != nil {
				return relErr
			}
			report.orphanFullPaths = append(report.orphanFullPaths, path)
			if len(report.OrphanSidecars) < MaxOrphanSidecarSamples {
				report.OrphanSidecars = append(report.OrphanSidecars, filepath.ToSlash(relative))
			}
			return nil
		}
		if excludedObjectFile(name, metadataSuffix) || !entry.Type().IsRegular() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		report.ObjectCount++
		report.ScannedBytes += info.Size()
		return nil
	})
	return report, err
}

func (r ReconcileReport) OrphanSidecarCount() int { return len(r.orphanFullPaths) }

func (r ReconcileReport) DeleteOrphanSidecars() (int, error) {
	deleted := 0
	for _, path := range r.orphanFullPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func excludedObjectFile(name, metadataSuffix string) bool {
	return strings.HasSuffix(name, metadataSuffix) || strings.HasSuffix(name, ".s3meta") || strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3")
}
