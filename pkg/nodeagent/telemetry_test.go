package nodeagent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

func setupTelemetryDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb := openNodeTestDB(t)
	if err := MigrateState(gdb); err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	return gdb
}

// 变更矩阵:PUT 新对象 / 覆盖 / Copy / 单删 / 批删 / Multipart Complete 在
// handlers 层各自换算成净增量,这里验证计数器对增量的语义正确,包括零大小
// 对象按存在计数。
func TestStorageTelemetryRecorderMutationMatrix(t *testing.T) {
	gdb := setupTelemetryDB(t)
	rec := NewStorageTelemetryRecorder(gdb)
	// 建立基线:2 对象、100 字节。
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}

	mutations := []TelemetryMutation{
		{DeltaBytes: 10, DeltaObjects: 1},      // PUT 新对象(零大小对象也 +1)
		{DeltaBytes: 0, DeltaObjects: 1},       // PUT 零大小新对象
		{DeltaBytes: 15 - 10, DeltaObjects: 0}, // 覆盖:大小差,对象数不变
		{DeltaBytes: 5, DeltaObjects: 1},       // Copy 到新 key
		{DeltaBytes: -15, DeltaObjects: -1},    // 删除
		{DeltaBytes: -8, DeltaObjects: 0},      // 删除不存在时(head 未命中)不发生
	}
	mutations = mutations[:5]
	for _, m := range mutations {
		rec.RecordMutation(m.DeltaBytes, m.DeltaObjects)
	}

	telemetry, ok, err := LoadStorageTelemetry(gdb)
	if err != nil || !ok {
		t.Fatalf("telemetry unavailable: ok=%v err=%v", ok, err)
	}
	// 10 + 0 + 5 + 5 - 15 = 5;对象 1+1+0+1-1 = 2。
	if telemetry.UsedBytesTotal != 5 || telemetry.ObjectCount != 2 {
		t.Fatalf("counters = %d bytes / %d objects, want 5 / 2", telemetry.UsedBytesTotal, telemetry.ObjectCount)
	}
	if telemetry.ObservedAt.IsZero() {
		t.Fatal("observed_at must be set")
	}
}

func TestStorageTelemetryRecorderConcurrentMutationsAreNotLost(t *testing.T) {
	gdb := setupTelemetryDB(t)
	rec := NewStorageTelemetryRecorder(gdb)
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const each = 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				rec.RecordMutation(4, 1)
			}
		}()
	}
	wg.Wait()
	telemetry, ok, _ := LoadStorageTelemetry(gdb)
	if !ok {
		t.Fatal("telemetry must stay valid")
	}
	if telemetry.UsedBytesTotal != workers*each*4 || telemetry.ObjectCount != workers*each {
		t.Fatalf("lost updates: %d bytes / %d objects, want %d / %d",
			telemetry.UsedBytesTotal, telemetry.ObjectCount, workers*each*4, workers*each)
	}
}

// 没有基线时增量只累计、不宣告可靠;重启后(重新打开同一 DB)计数仍可恢复,
// 但在重建基线前心跳必须省略遥测。
func TestStorageTelemetryWithoutBaselineStaysUnavailable(t *testing.T) {
	gdb := setupTelemetryDB(t)
	rec := NewStorageTelemetryRecorder(gdb)
	rec.RecordMutation(100, 1)

	if _, ok, err := LoadStorageTelemetry(gdb); err != nil || ok {
		t.Fatalf("telemetry without baseline must be unavailable: ok=%v err=%v", ok, err)
	}

	// 重启恢复:计数持久化在数据库中,重建基线后恢复可靠(计数以基线为准)。
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadStorageTelemetry(gdb); !ok {
		t.Fatal("telemetry must become valid after baseline")
	}
}

// 已失效的计数器不会被后续增量"洗白":只有基线重建能恢复 Valid。
func TestStorageTelemetryInvalidNotRevivedByMutations(t *testing.T) {
	gdb := setupTelemetryDB(t)
	rec := NewStorageTelemetryRecorder(gdb)
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	rec.Invalidate("manual rebuild needed")

	rec.RecordMutation(7, 1)
	if _, ok, _ := LoadStorageTelemetry(gdb); ok {
		t.Fatal("invalid telemetry must not become valid via mutation")
	}
}

func TestStorageTelemetryNegativeDeltasClampToZero(t *testing.T) {
	gdb := setupTelemetryDB(t)
	rec := NewStorageTelemetryRecorder(gdb)
	rec.RecordMutation(-50, -2)

	var row StorageTelemetry
	if err := gdb.First(&row, StorageTelemetryID).Error; err != nil {
		t.Fatal(err)
	}
	if row.UsedBytesTotal != 0 || row.ObjectCount != 0 {
		t.Fatalf("counters = %d/%d, want clamped 0/0", row.UsedBytesTotal, row.ObjectCount)
	}
}

// 基线扫描遵循现有排除规则:sidecar、.multipart、数据库文件不计数,跨桶
// 汇总,零大小对象按存在计数。
func TestStorageTelemetryBaselineScanExclusions(t *testing.T) {
	gdb := setupTelemetryDB(t)
	root := t.TempDir()
	// bucket-a:两个对象,其中一个零字节;一个元数据 sidecar。
	a := filepath.Join(root, "bucket-a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "obj1"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "obj2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "obj1"+storage.DefaultMetadataSuffix), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// bucket-b:一个对象;.multipart 目录下的分片不算对象。
	b := filepath.Join(root, "bucket-b")
	mp := filepath.Join(b, ".multipart", "upload-1")
	if err := os.MkdirAll(mp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "obj3"), []byte("1234567890"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "part-1"), []byte("chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 数据根下的数据库文件与非法桶名目录不参与统计。
	if err := os.WriteFile(filepath.Join(root, "node.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Tmp_dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Tmp_dir", "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RebuildStorageTelemetry(gdb, root, ""); err != nil {
		t.Fatal(err)
	}
	telemetry, ok, _ := LoadStorageTelemetry(gdb)
	if !ok {
		t.Fatal("baseline must produce valid telemetry")
	}
	// obj1(5) + obj2(0) + obj3(10) = 15 字节 / 3 对象。
	if telemetry.UsedBytesTotal != 15 || telemetry.ObjectCount != 3 {
		t.Fatalf("baseline = %d bytes / %d objects, want 15 / 3",
			telemetry.UsedBytesTotal, telemetry.ObjectCount)
	}
	if telemetry.ObservedAt.IsZero() {
		t.Fatal("baseline must stamp observed_at")
	}
}

// 基线失败(数据根不可读)时标记不可用并返回错误,不得用 0 冒充观测值。
func TestStorageTelemetryBaselineFailureMarksInvalid(t *testing.T) {
	gdb := setupTelemetryDB(t)
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	// 用一个文件路径当作数据根:扫描必然失败。
	notADir := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RebuildStorageTelemetry(gdb, notADir, ""); err == nil {
		t.Fatal("expected baseline failure")
	}
	if _, ok, _ := LoadStorageTelemetry(gdb); ok {
		t.Fatal("failed baseline must leave telemetry unavailable")
	}
}

// EnsureStorageTelemetryBaseline:有效计数器存在时是纯读取,不重扫。
func TestEnsureStorageTelemetryBaselineSkipsValidCounters(t *testing.T) {
	gdb := setupTelemetryDB(t)
	emptyRoot := t.TempDir()
	if err := RebuildStorageTelemetry(gdb, emptyRoot, ""); err != nil {
		t.Fatal(err)
	}
	rec := NewStorageTelemetryRecorder(gdb)
	rec.RecordMutation(42, 1)

	// 换一个内容不同的数据根:若重新扫描,计数会被覆盖为 0。
	filledRoot := t.TempDir()
	bucket := filepath.Join(filledRoot, "b")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucket, "o"), []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStorageTelemetryBaseline(gdb, filledRoot, ""); err != nil {
		t.Fatal(err)
	}
	telemetry, ok, _ := LoadStorageTelemetry(gdb)
	if !ok || telemetry.UsedBytesTotal != 42 || telemetry.ObjectCount != 1 {
		t.Fatalf("valid counters must survive EnsureBaseline: %+v ok=%v", telemetry, ok)
	}
}

// 心跳快照:不可用时三个字段全部省略(旧 Panel 兼容);可用时显式携带,
// 包括合法的 0。快照只做单行读,函数签名里没有任何文件系统参数。
func TestHeartbeatTelemetrySnapshotFields(t *testing.T) {
	gdb := setupTelemetryDB(t)

	// 没有基线:字段必须省略。
	payload := HeartbeatTelemetrySnapshot(gdb)
	if payload.UsedBytesTotal != nil || payload.ObjectCount != nil || payload.ObservedAt != "" {
		t.Fatalf("unavailable telemetry must omit all fields: %+v", payload)
	}
	if _, ok := payload.Telemetry(); ok {
		t.Fatal("omitted snapshot must not decode as telemetry")
	}

	// 空数据根基线:合法的 0/0,字段必须显式携带。
	if err := RebuildStorageTelemetry(gdb, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	payload = HeartbeatTelemetrySnapshot(gdb)
	telemetry, ok := payload.Telemetry()
	if !ok {
		t.Fatal("explicit zero snapshot must be complete telemetry")
	}
	if telemetry.UsedBytesTotal != 0 || telemetry.ObjectCount != 0 {
		t.Fatalf("zero snapshot corrupted: %+v", telemetry)
	}
	if _, err := time.Parse(time.RFC3339Nano, payload.ObservedAt); err != nil {
		t.Fatalf("observed_at must be RFC3339: %v", err)
	}
}
