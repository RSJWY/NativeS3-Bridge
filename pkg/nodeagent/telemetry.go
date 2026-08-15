package nodeagent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	// ObservedAt 是计数器对应的观测时间(节点时钟,UTC)。计数器有效时它由
	// 心跳读取推进(见 HeartbeatTelemetrySnapshot),因此空闲节点不会过期。
	ObservedAt time.Time
	Valid      bool `gorm:"not null;default:false"`
	UpdatedAt  time.Time
}

// StorageTelemetryRecorder 维护节点级存储遥测计数器,并实现 handlers 的
// TelemetryRecorder 接口。成功变更存储后的增量更新在这里落库;更新失败只
// 失效遥测,绝不把已成功的 S3 请求变成错误或伪造 0。
type StorageTelemetryRecorder struct {
	db       *gorm.DB
	dataRoot string

	// mutationGate keeps native storage mutations and their counter updates in
	// one shared critical section. A rebuild takes the exclusive side, so its
	// filesystem scan cannot overlap an object commit or the following DB write.
	mutationGate sync.RWMutex
	// counterMu serializes the singleton row's read-modify-write transaction.
	// This is portable across SQLite, MySQL, and PostgreSQL without relying on
	// driver-specific locking clauses.
	counterMu sync.Mutex
	// invalid is the immediate fail-closed latch. The marker under dataRoot
	// makes the same state survive process restarts even if the DB invalidation
	// write itself failed.
	invalid atomic.Bool
}

const storageTelemetryInvalidMarker = ".natives3-telemetry-invalid"

func NewStorageTelemetryRecorder(gdb *gorm.DB, dataRoot ...string) *StorageTelemetryRecorder {
	r := &StorageTelemetryRecorder{db: gdb}
	if len(dataRoot) > 0 {
		r.dataRoot = dataRoot[0]
		if exists, err := telemetryInvalidMarkerExists(r.dataRoot); err != nil || exists {
			r.invalid.Store(true)
			if err != nil {
				slog.Warn("inspect storage telemetry invalid marker failed", "error", err)
			}
		}
	}
	return r
}

// BeginMutation joins a native object change and its telemetry accounting into
// the rebuild gate's shared side. Handlers hold the returned guard from before
// their existence check until after RecordMutation returns.
func (r *StorageTelemetryRecorder) BeginMutation() func() {
	if r == nil {
		return func() {}
	}
	r.mutationGate.RLock()
	return r.mutationGate.RUnlock
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
// 自己的净增量;进程内串行化加事务保证跨数据库驱动不丢更新。增量把计数器
// 推成负数或溢出时显式失效,绝不把截断后的 0 当成有效观测值。
func (r *StorageTelemetryRecorder) RecordMutation(deltaBytes, deltaObjects int64) {
	if r == nil || r.db == nil {
		return
	}
	r.counterMu.Lock()
	defer r.counterMu.Unlock()
	now := time.Now().UTC()
	var err error
	var counterInvalid bool
	for attempt := 0; ; attempt++ {
		counterInvalid, err = r.applyMutation(deltaBytes, deltaObjects, now)
		if err == nil || attempt >= telemetryBusyAttempts || !isSQLiteBusy(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	if err != nil {
		// 原生存储已提交,但计数器无法推进:整份遥测不可信,显式失效,
		// 等待基线/显式 reconcile 重建,绝不静默误报。
		slog.Error("record storage telemetry mutation failed; invalidating telemetry", "error", err)
		r.invalidate("telemetry accounting failed")
	} else if counterInvalid {
		r.invalidate("storage telemetry counter became invalid")
	}
}

func (r *StorageTelemetryRecorder) applyMutation(deltaBytes, deltaObjects int64, now time.Time) (bool, error) {
	counterInvalid := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
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
		newBytes, bytesOverflow := addChecked(row.UsedBytesTotal, deltaBytes)
		newObjects, objectsOverflow := addChecked(row.ObjectCount, deltaObjects)
		updates := map[string]any{
			"used_bytes_total": newBytes,
			"object_count":     newObjects,
			"observed_at":      now,
			"updated_at":       now,
		}
		if bytesOverflow || objectsOverflow || newBytes < 0 || newObjects < 0 {
			counterInvalid = true
			// 负数或溢出说明基线或记账已经失真:截断只保留可读的行内容,
			// 必须显式失效,绝不能带着 Valid 把异常计数当成观测值上报。
			slog.Error("storage telemetry delta drove counter invalid; invalidating telemetry",
				"used_bytes_total", row.UsedBytesTotal, "delta_bytes", deltaBytes,
				"object_count", row.ObjectCount, "delta_objects", deltaObjects)
			updates["used_bytes_total"] = clampNonNegative(newBytes)
			updates["object_count"] = clampNonNegative(newObjects)
			updates["valid"] = false
		}
		// 沿用行的 Valid(除非上面显式失效):已失效的计数器不会被增量"洗白",
		// 只有基线重建或显式修复能恢复可靠性。
		return tx.Model(&StorageTelemetry{}).Where("id = ?", StorageTelemetryID).Updates(updates).Error
	})
	return counterInvalid, err
}

// Invalidate 显式把计数器标记为不可用(Valid=false)。行内容保留供排查。
func (r *StorageTelemetryRecorder) Invalidate(reason string) {
	if r == nil {
		return
	}
	r.counterMu.Lock()
	defer r.counterMu.Unlock()
	r.invalidate(reason)
}

func (r *StorageTelemetryRecorder) invalidate(reason string) {
	if r == nil || r.db == nil {
		return
	}
	r.invalid.Store(true)
	if r.dataRoot != "" {
		if err := writeTelemetryInvalidMarker(r.dataRoot, reason); err != nil {
			slog.Warn("write storage telemetry invalid marker failed", "reason", reason, "error", err)
		}
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
	if !row.Valid || row.ObservedAt.IsZero() || row.UsedBytesTotal < 0 || row.ObjectCount < 0 {
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
// 可用时把 observed_at 推进到本次心跳时间:计数器由增量事务持续维护,空闲
// 节点不会发生存储变更,若沿用最后一次变更的时间,Panel 会把健康空闲节点
// 误判为遥测过期。
func HeartbeatTelemetrySnapshot(gdb *gorm.DB) controlproto.HeartbeatPayload {
	return NewStorageTelemetryRecorder(gdb).HeartbeatTelemetrySnapshot()
}

// HeartbeatTelemetrySnapshot returns the latest valid row without scanning the
// filesystem. A recorder-level invalid latch takes precedence over a stale DB
// row when the previous invalidation could not be persisted.
func (r *StorageTelemetryRecorder) HeartbeatTelemetrySnapshot() controlproto.HeartbeatPayload {
	payload := controlproto.HeartbeatPayload{}
	if r == nil || r.db == nil || r.invalid.Load() {
		return payload
	}
	telemetry, ok, err := LoadStorageTelemetry(r.db)
	if err != nil {
		slog.Warn("heartbeat: load storage telemetry failed", "error", err)
		return payload
	}
	if !ok {
		return payload
	}
	now := time.Now().UTC()
	// 单行常量级写,只刷新观测时间,不动计数与世代;失败时按旧观测时间上报,
	// Panel 的过期判断自然兜底。
	if err := r.db.Model(&StorageTelemetry{}).Where("id = ?", StorageTelemetryID).
		Updates(map[string]any{"observed_at": now, "updated_at": now}).Error; err != nil {
		slog.Warn("heartbeat: refresh telemetry observed_at failed", "error", err)
	}
	payload.UsedBytesTotal = &telemetry.UsedBytesTotal
	payload.ObjectCount = &telemetry.ObjectCount
	payload.ObservedAt = now.Format(time.RFC3339Nano)
	return payload
}

// RebuildStorageTelemetry 用一次同步全量扫描重建节点级遥测基线。启动阶段
// (尚未接受 S3 流量)或显式 reconcile 修复路径都可以调用。共享 recorder 的
// 独占门闩覆盖整个扫描与落库过程;handlers 的存储变更持有共享门闩直到增量
// 记账完成,因此不存在文件已写入而计数尚未推进的交接窗口。
func RebuildStorageTelemetry(gdb *gorm.DB, dataRoot, metadataSuffix string) error {
	return NewStorageTelemetryRecorder(gdb, dataRoot).RebuildStorageTelemetry(dataRoot, metadataSuffix)
}

func (r *StorageTelemetryRecorder) RebuildStorageTelemetry(dataRoot, metadataSuffix string) error {
	if r == nil || r.db == nil {
		return errors.New("storage telemetry recorder is not configured")
	}
	if dataRoot == "" {
		dataRoot = r.dataRoot
	}
	r.mutationGate.Lock()
	defer r.mutationGate.Unlock()
	r.counterMu.Lock()
	defer r.counterMu.Unlock()
	r.invalid.Store(true)
	if err := writeTelemetryInvalidMarker(dataRoot, "storage telemetry rebuild in progress"); err != nil {
		r.invalidate("write storage telemetry rebuild marker failed")
		return fmt.Errorf("write storage telemetry rebuild marker: %w", err)
	}

	report, err := storage.ScanDataRoot(dataRoot, metadataSuffix)
	if err != nil {
		r.invalidate("storage telemetry baseline scan failed")
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
	if err := r.db.Save(&row).Error; err != nil {
		r.invalidate("persist storage telemetry baseline failed")
		return fmt.Errorf("persist storage telemetry baseline: %w", err)
	}
	if err := clearTelemetryInvalidMarker(dataRoot); err != nil {
		r.invalidate("clear storage telemetry invalid marker failed")
		return fmt.Errorf("clear storage telemetry invalid marker: %w", err)
	}
	r.invalid.Store(false)
	slog.Info("storage telemetry baseline rebuilt",
		"used_bytes_total", report.ScannedBytes, "object_count", report.ObjectCount)
	return nil
}

// EnsureStorageTelemetryBaseline 在没有有效基线时执行一次同步扫描。已有
// 有效计数器时是纯单行读,节点重启后直接恢复最新计数。
func EnsureStorageTelemetryBaseline(gdb *gorm.DB, dataRoot, metadataSuffix string) error {
	return NewStorageTelemetryRecorder(gdb, dataRoot).EnsureStorageTelemetryBaseline(dataRoot, metadataSuffix)
}

func (r *StorageTelemetryRecorder) EnsureStorageTelemetryBaseline(dataRoot, metadataSuffix string) error {
	if r == nil || r.db == nil {
		return errors.New("storage telemetry recorder is not configured")
	}
	if !r.invalid.Load() {
		if _, ok, err := LoadStorageTelemetry(r.db); err != nil {
			return err
		} else if ok {
			return nil
		}
	}
	return r.RebuildStorageTelemetry(dataRoot, metadataSuffix)
}

func telemetryInvalidMarkerPath(dataRoot string) string {
	return filepath.Join(dataRoot, storageTelemetryInvalidMarker)
}

func telemetryInvalidMarkerExists(dataRoot string) (bool, error) {
	if dataRoot == "" {
		return false, nil
	}
	_, err := os.Stat(telemetryInvalidMarkerPath(dataRoot))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func writeTelemetryInvalidMarker(dataRoot, reason string) error {
	if dataRoot == "" {
		return nil
	}
	return os.WriteFile(telemetryInvalidMarkerPath(dataRoot), []byte(reason+"\n"), 0o600)
}

func clearTelemetryInvalidMarker(dataRoot string) error {
	if dataRoot == "" {
		return nil
	}
	err := os.Remove(telemetryInvalidMarkerPath(dataRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// addChecked 带溢出检测的加法:和回绕即判定计数不可靠,由调用方显式失效。
func addChecked(base, delta int64) (int64, bool) {
	sum := base + delta
	if (delta > 0 && sum < base) || (delta < 0 && sum > base) {
		return sum, true
	}
	return sum, false
}
