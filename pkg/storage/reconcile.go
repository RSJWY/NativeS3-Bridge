package storage

import (
	"context"
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

// ReconcileBucket 扫描单个桶目录并返回统计报告。它是 context-free 兼容入口,
// 内部走 context.Background(),便于未接入 ctx 的现有调用方直接使用。
func ReconcileBucket(root, bucket, metadataSuffix string) (ReconcileReport, error) {
	return ReconcileBucketContext(context.Background(), root, bucket, metadataSuffix)
}

// ReconcileBucketContext 是 ReconcileBucket 的 context 版本,允许调用方在扫描
// 过程中取消操作(例如 panel 下发的任务超时)。
func ReconcileBucketContext(ctx context.Context, root, bucket, metadataSuffix string) (ReconcileReport, error) {
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
	err = walkBucket(ctx, bucketPath, metadataSuffix, &report)
	return report, err
}

// ScanDataRoot 全量扫描数据目录下所有桶,汇总节点级对象数与字节数。它是节点
// 遥测基线/重建的采集入口:逐个遍历根目录下的合法桶目录,复用 walkBucket 的
// 排除规则(sidecar、.multipart、数据库文件),不做任何删除或记账变更。
func ScanDataRoot(root, metadataSuffix string) (ReconcileReport, error) {
	return ScanDataRootContext(context.Background(), root, metadataSuffix)
}

// ScanDataRootContext 是 ScanDataRoot 的 context 版本。
func ScanDataRootContext(ctx context.Context, root, metadataSuffix string) (ReconcileReport, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return ReconcileReport{}, err
	}
	if metadataSuffix == "" {
		metadataSuffix = DefaultMetadataSuffix
	}
	report := ReconcileReport{Bucket: ""}
	entries, err := os.ReadDir(rootAbs)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return ReconcileReport{}, err
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ReconcileReport{}, ctx.Err()
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		// 非法桶名目录(运维临时目录等)不属于对象存储命名空间,直接跳过。
		if err := ValidateBucketName(entry.Name()); err != nil {
			continue
		}
		bucketReport := ReconcileReport{Bucket: entry.Name()}
		if err := walkBucket(ctx, filepath.Join(rootAbs, entry.Name()), metadataSuffix, &bucketReport); err != nil {
			return ReconcileReport{}, err
		}
		report.ObjectCount += bucketReport.ObjectCount
		report.ScannedBytes += bucketReport.ScannedBytes
	}
	return report, nil
}

// walkBucket 统计单个桶目录下的对象数与字节数,跳过元数据 sidecar、.multipart
// 与数据库文件。ReconcileBucket 与 ScanDataRoot 共用它,保证两套口径一致。
// 每轮 DirEntry 回调检查 ctx,支持可取消扫描。
func walkBucket(ctx context.Context, bucketPath, metadataSuffix string, report *ReconcileReport) error {
	err := filepath.WalkDir(bucketPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
	return err
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
