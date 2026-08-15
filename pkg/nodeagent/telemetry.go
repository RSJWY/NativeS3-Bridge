package nodeagent

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

// StorageTelemetryID 是节点级存储遥测的单例行主键。单例意味着"最新一份观
// 测",不做历史趋势或时序表。
const StorageTelemetryID = 1

// StorageTelemetry 是节点级存储使用量的最新观测快照(单例行)。它独立于凭据
// 配额记账:credentials.used_bytes 是配额视图,跨凭据覆盖/删除会失真,节点
// 总量必须由自己的计数器维护。Valid=false 表示计数器不可靠(基线未建立、
// 记账失败或外部改盘),此时心跳必须省略遥测字段而不是上报 0。
type StorageTelemetry struct {
	ID             uint  `gorm:"primaryKey"`
	UsedBytesTotal int64 `gorm:"not null;default:0"`
	ObjectCount    int64 `gorm:"not null;default:0"`
	// ObservedAt 是计数器对应的观测时间(节点时钟,UTC),不是心跳发送时间。
	ObservedAt time.Time
	Valid      bool `gorm:"not null;default:false"`
	UpdatedAt  time.Time
}

// StorageTelemetryRecorder 维护节点级存储遥测计数器,并实现 handlers 的
// TelemetryRecorder 接口。成功变更存储后的增量更新在这里落库;更新失败只
// 失效遥测,绝不把已成功的 S3 请求变成错误或伪造 0。
type StorageTelemetryRecorder struct {
	db *gorm.DB
}

func NewStorageTelemetryRecorder(gdb *gorm.DB) *StorageTelemetryRecorder {
	return &StorageTelemetryRecorder{db: gdb}
}

// TelemetryMutation 描述一次成功存储变更对节点级计数器的净影响。
// DeltaObjects 只可能是 -1/0/+1(零大小对象按"是否存在"计数,不看大小)。
type TelemetryMutation struct {
	DeltaBytes   int64
	DeltaObjects int64
}

// telemetryBusyAttempts bounds SQLITE_BUSY retries for the counter transaction.
// SQLite 只有一个写者:并发的 S3 变更短暂锁库是常态,忙等重试不能触发失效。
const telemetryBusyAttempts = 20

func isSQLiteBusy(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY / SQLITE_LOCKED
		return true
	default:
		return false
	}
}

// RecordMutation 在一次成功存储变更后原子地推进计数器。并发请求各自携带
// 自己的净增量;事务内读改写保证不丢更新。增量把计数器推成负数时按 0 截断
// 并告警:负数说明基线或记账已失真,但轻微竞态不应直接摧毁遥测可用性。
func (r *StorageTelemetryRecorder) RecordMutation(deltaBytes, deltaObjects int64) {
	if r == nil || r.db == nil {
		return
	}
	now := time.Now().UTC()
	var err error
	for attempt := 0; ; attempt++ {
		err = r.applyMutation(deltaBytes, deltaObjects, now)
		if err == nil || attempt >= telemetryBusyAttempts || !isSQLiteBusy(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	if err != nil {
		// 原生存储已提交,但计数器无法推进:整份遥测不可信,显式失效,
		// 等待基线/显式 reconcile 重建,绝不静默误报。
		slog.Error("record storage telemetry mutation failed; invalidating telemetry", "error", err)
		r.Invalidate("telemetry accounting failed")
	}
}

func (r *StorageTelemetryRecorder) applyMutation(deltaBytes, deltaObjects int64, now time.Time) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var row StorageTelemetry
		err := tx.First(&row, StorageTelemetryID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 没有基线行时只累计增量,不置 Valid:计数缺少既有数据的初值,
			// 上报它就是伪造。RebuildStorageTelemetry 才能宣告可靠。
			return tx.Create(&StorageTelemetry{
				ID:             StorageTelemetryID,
				UsedBytesTotal: clampNonNegative(deltaBytes),
				ObjectCount:    clampNonNegative(deltaObjects),
				ObservedAt:     now,
				Valid:          false,
				UpdatedAt:      now,
			}).Error
		}
		if err != nil {
			return err
		}
		newBytes := row.UsedBytesTotal + deltaBytes
		newObjects := row.ObjectCount + deltaObjects
		if newBytes < 0 || newObjects < 0 {
			slog.Warn("storage telemetry delta drove counter negative; clamping",
				"used_bytes_total", row.UsedBytesTotal, "delta_bytes", deltaBytes,
				"object_count", row.ObjectCount, "delta_objects", deltaObjects)
			newBytes = clampNonNegative(newBytes)
			newObjects = clampNonNegative(newObjects)
		}
		// 沿用行的 Valid:已失效的计数器不会被增量"洗白",只有基线重建或
		// 显式修复能恢复可靠性。
		return tx.Model(&StorageTelemetry{}).Where("id = ?", StorageTelemetryID).Updates(map[string]any{
			"used_bytes_total": newBytes,
			"object_count":     newObjects,
			"observed_at":      now,
			"updated_at":       now,
		}).Error
	})
}

// Invalidate 显式把计数器标记为不可用(Valid=false)。行内容保留供排查。
func (r *StorageTelemetryRecorder) Invalidate(reason string) {
	if r == nil || r.db == nil {
		return
	}
	err := r.db.Model(&StorageTelemetry{}).Where("id = ?", StorageTelemetryID).Updates(map[string]any{
		"valid":      false,
		"updated_at": time.Now().UTC(),
	}).Error
	if err != nil {
		slog.Warn("invalidate storage telemetry failed", "reason", reason, "error", err)
		return
	}
	slog.Warn("storage telemetry marked invalid", "reason", reason)
}

// LoadStorageTelemetry 读取最新观测快照。这是一次常量级单行读:心跳路径只
// 调用它,绝不触发任何文件系统扫描。ok=false 表示不可用或尚未建立,调用方
// 必须省略遥测字段。
func LoadStorageTelemetry(gdb *gorm.DB) (controlproto.HeartbeatTelemetry, bool, error) {
	var row StorageTelemetry
	err := gdb.First(&row, StorageTelemetryID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return controlproto.HeartbeatTelemetry{}, false, nil
	}
	if err != nil {
		return controlproto.HeartbeatTelemetry{}, false, err
	}
	if !row.Valid || row.ObservedAt.IsZero() {
		return controlproto.HeartbeatTelemetry{}, false, nil
	}
	return controlproto.HeartbeatTelemetry{
		UsedBytesTotal: row.UsedBytesTotal,
		ObjectCount:    row.ObjectCount,
		ObservedAt:     row.ObservedAt,
	}, true, nil
}

// HeartbeatTelemetrySnapshot 组装心跳遥测字段。不可用时三个字段全部留空,
// 让 omitempty 在线上把它们省略,旧 Panel 与新 Panel 都把它解释为"未上报"。
func HeartbeatTelemetrySnapshot(gdb *gorm.DB) controlproto.HeartbeatPayload {
	payload := controlproto.HeartbeatPayload{}
	telemetry, ok, err := LoadStorageTelemetry(gdb)
	if err != nil {
		slog.Warn("heartbeat: load storage telemetry failed", "error", err)
		return payload
	}
	if !ok {
		return payload
	}
	payload.UsedBytesTotal = &telemetry.UsedBytesTotal
	payload.ObjectCount = &telemetry.ObjectCount
	payload.ObservedAt = telemetry.ObservedAt.Format(time.RFC3339Nano)
	return payload
}

// RebuildStorageTelemetry 用一次同步全量扫描重建节点级遥测基线。它只在节点
// 尚未接受 S3 流量的启动阶段(或显式 reconcile 修复路径)调用:全量扫描与在
// 线写入并发会产生计数竞态。扫描失败时标记不可用并返回错误,由调用方决定
// 是否继续提供服务;任何情况下都不会用 0 冒充观测值。
func RebuildStorageTelemetry(gdb *gorm.DB, dataRoot, metadataSuffix string) error {
	report, err := storage.ScanDataRoot(dataRoot, metadataSuffix)
	if err != nil {
		if invalidateErr := markStorageTelemetryInvalid(gdb); invalidateErr != nil {
			slog.Warn("mark storage telemetry invalid failed", "error", invalidateErr)
		}
		return fmt.Errorf("scan storage root: %w", err)
	}
	now := time.Now().UTC()
	row := StorageTelemetry{
		ID:             StorageTelemetryID,
		UsedBytesTotal: report.ScannedBytes,
		ObjectCount:    report.ObjectCount,
		ObservedAt:     now,
		Valid:          true,
		UpdatedAt:      now,
	}
	if err := gdb.Save(&row).Error; err != nil {
		return fmt.Errorf("persist storage telemetry baseline: %w", err)
	}
	slog.Info("storage telemetry baseline rebuilt",
		"used_bytes_total", report.ScannedBytes, "object_count", report.ObjectCount)
	return nil
}

// EnsureStorageTelemetryBaseline 在没有有效基线时执行一次同步扫描。已有
// 有效计数器时是纯单行读,节点重启后直接恢复最新计数。
func EnsureStorageTelemetryBaseline(gdb *gorm.DB, dataRoot, metadataSuffix string) error {
	if _, ok, err := LoadStorageTelemetry(gdb); err != nil {
		return err
	} else if ok {
		return nil
	}
	return RebuildStorageTelemetry(gdb, dataRoot, metadataSuffix)
}

func markStorageTelemetryInvalid(gdb *gorm.DB) error {
	return gdb.Model(&StorageTelemetry{}).Where("id = ?", StorageTelemetryID).Updates(map[string]any{
		"valid":      false,
		"updated_at": time.Now().UTC(),
	}).Error
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
