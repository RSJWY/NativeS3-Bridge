package panel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// R5(AC4):正常 cadence 下每帧都落库——落库行为必须与加节流之前逐帧一致,
// 否则 SweepOffline 的 DB 口径会变旧。
func TestHeartbeatPersistsEveryFrameAtNormalCadence(t *testing.T) {
	conn := newAgentConn(1, "fp", nil)
	interval := 15 * time.Second
	minInterval := interval / 2

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if !conn.shouldPersistHeartbeat(now, minInterval) {
			t.Fatalf("frame #%d at normal cadence was throttled; DB last_heartbeat would go stale", i)
		}
		now = now.Add(interval)
	}
}

// R5(AC4):狂发心跳时落库频率被压到阈值以下(上限 2/interval),而节流的滞后量远小于
// SweepOffline 的离线阈值,活跃节点不会被误判 offline。
func TestHeartbeatFloodIsThrottledWellBelowOfflineThreshold(t *testing.T) {
	conn := newAgentConn(1, "fp", nil)
	interval := 15 * time.Second
	minInterval := interval / 2

	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	now := start
	persisted := 0
	var lastPersist time.Time
	maxGap := time.Duration(0)

	// 100ms 一帧狂发 60s。
	for now.Sub(start) < time.Minute {
		if conn.shouldPersistHeartbeat(now, minInterval) {
			if !lastPersist.IsZero() && now.Sub(lastPersist) > maxGap {
				maxGap = now.Sub(lastPersist)
			}
			lastPersist = now
			persisted++
		}
		now = now.Add(100 * time.Millisecond)
	}

	// 60s / 7.5s = 8 次上限(首帧直接落库,故 ≤ 9)。未节流时会是 600 次。
	if persisted > 9 {
		t.Fatalf("flood persisted %d times in 60s; throttle is not bounding DB writes", persisted)
	}
	if persisted == 0 {
		t.Fatal("flood persisted nothing; liveness would go stale")
	}

	// 关键安全不变性:落库间隔必须远小于离线阈值(offline_multiplier * interval)。
	offlineThreshold := time.Duration(DefaultOfflineMultiplier) * interval
	if maxGap >= offlineThreshold {
		t.Fatalf("persist gap %v reached the offline threshold %v; a live node could be marked offline",
			maxGap, offlineThreshold)
	}
}

// R5:阈值随实际配置的 heartbeat_interval 走,而不是写死默认值——否则调大/调小
// cadence 的部署会按错误的阈值节流。
func TestHeartbeatPersistIntervalFollowsConfiguredCadence(t *testing.T) {
	custom := NewTransportServer(TransportDeps{HeartbeatInterval: 60 * time.Second})
	if got := custom.heartbeatPersistInterval(); got != 30*time.Second {
		t.Fatalf("persist interval = %v, want half of the configured 60s cadence", got)
	}
	defaulted := NewTransportServer(TransportDeps{})
	if got := defaulted.heartbeatPersistInterval(); got != DefaultHeartbeatInterval/2 {
		t.Fatalf("persist interval = %v, want half of the default cadence", got)
	}
}

// R6(AC5):单次瞬时存储错误不拆连接;同一连接连续失败到阈值才断开,避免挂着一条
// 永远写不进库的连接。
func TestTransientStorageErrorsKeepConnectionUntilThreshold(t *testing.T) {
	server := NewTransportServer(TransportDeps{})
	conn := newAgentConn(7, "fp", nil)
	boom := errors.New("database is locked")

	for i := 1; i < maxConsecutiveStorageFailures; i++ {
		if err := server.noteStorageFailure(conn, "persist test", boom); err != nil {
			t.Fatalf("failure #%d returned %v; a transient error must not drop the connection", i, err)
		}
	}
	err := server.noteStorageFailure(conn, "persist test", boom)
	if err == nil {
		t.Fatalf("failure #%d must trip the threshold and close the connection", maxConsecutiveStorageFailures)
	}
	if !errors.Is(err, errPersistentStorageFailure) {
		t.Fatalf("threshold error = %v, want it to wrap errPersistentStorageFailure", err)
	}
}

// R6:阈值只针对"连续"失败——一次成功落库后计数必须清零,否则长连接上零星的瞬时
// 错误累积起来仍会把健康连接踢掉。
func TestStorageFailureCounterResetsOnSuccess(t *testing.T) {
	server := NewTransportServer(TransportDeps{})
	conn := newAgentConn(7, "fp", nil)
	boom := errors.New("database is locked")

	for i := 0; i < maxConsecutiveStorageFailures-1; i++ {
		if err := server.noteStorageFailure(conn, "persist test", boom); err != nil {
			t.Fatalf("unexpected drop at failure #%d: %v", i+1, err)
		}
	}
	conn.noteStorageSuccess()

	// 清零后又能容忍完整的一轮失败。
	for i := 0; i < maxConsecutiveStorageFailures-1; i++ {
		if err := server.noteStorageFailure(conn, "persist test", boom); err != nil {
			t.Fatalf("counter did not reset: dropped at failure #%d after a success: %v", i+1, err)
		}
	}
}

// R6:ctx 取消是连接正常关闭,不是存储故障——不能计入失败计数,否则一次正常断连就
// 污染了下一条连接的判定口径(并把 context 错误误报成存储故障)。
func TestContextCancellationIsNotCountedAsStorageFailure(t *testing.T) {
	server := NewTransportServer(TransportDeps{})
	conn := newAgentConn(7, "fp", nil)

	for i := 0; i < maxConsecutiveStorageFailures+3; i++ {
		err := server.noteStorageFailure(conn, "persist test", context.Canceled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ctx cancellation #%d returned %v, want it passed through unchanged", i, err)
		}
	}
	// 计数没被污染:仍能容忍完整的一轮真实失败。
	boom := errors.New("database is locked")
	for i := 0; i < maxConsecutiveStorageFailures-1; i++ {
		if err := server.noteStorageFailure(conn, "persist test", boom); err != nil {
			t.Fatalf("ctx cancellations polluted the counter: dropped at #%d: %v", i+1, err)
		}
	}
}

// R7(AC6):同一节点 1 小时内最多 10 次续期,第 11 次起 429 且带 Retry-After;
// 正常的 90 天一次续期完全不受影响。
func TestRenewLimiterBoundsPerNodeAndRecoversAfterWindow(t *testing.T) {
	limiter := newRenewLimiter()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	for i := 0; i < maxRenewPerWindow; i++ {
		if _, ok := limiter.allow(1, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("renew #%d was rejected below the limit", i+1)
		}
	}
	retryAfter, ok := limiter.allow(1, now.Add(time.Duration(maxRenewPerWindow)*time.Second))
	if ok {
		t.Fatalf("renew #%d must be rate limited", maxRenewPerWindow+1)
	}
	if retryAfter <= 0 {
		t.Fatalf("Retry-After = %v, want a positive hint", retryAfter)
	}

	// 另一个节点不受牵连(限频是按节点身份,不是全局)。
	if _, ok := limiter.allow(2, now); !ok {
		t.Fatal("a different node must not inherit another node's rate limit")
	}

	// 窗口滚过之后额度恢复——正常续期(每 90 天一次)永远不会撞到限制。
	if _, ok := limiter.allow(1, now.Add(renewWindow+time.Minute)); !ok {
		t.Fatal("quota must recover once the window rolls over")
	}
}

// R7:超限的尝试不入账,否则持续重试会不断把窗口往后推,变成越试越久的惩罚性锁定。
func TestRenewLimiterDoesNotExtendWindowOnRejectedAttempts(t *testing.T) {
	limiter := newRenewLimiter()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	for i := 0; i < maxRenewPerWindow; i++ {
		if _, ok := limiter.allow(1, now); !ok {
			t.Fatalf("renew #%d was rejected below the limit", i+1)
		}
	}
	// 在窗口内反复撞墙。
	for i := 0; i < 50; i++ {
		if _, ok := limiter.allow(1, now.Add(time.Duration(i)*time.Minute)); ok {
			t.Fatalf("attempt #%d unexpectedly allowed inside a saturated window", i)
		}
	}
	// 首批尝试滚出窗口后应立即恢复,而不是被这 50 次拒绝往后推。
	if _, ok := limiter.allow(1, now.Add(renewWindow+time.Second)); !ok {
		t.Fatal("rejected attempts must not extend the window")
	}
}

// R8(AC9):hello 自报的 content_hash 与 region 同口径消毒——控制字符剥离 + 按列宽
// 截断,避免脏值进管理端展示或撑坏列。
func TestReportedContentHashIsSanitized(t *testing.T) {
	dirty := "abc\x00def\x1b[31m\x7f" + strings.Repeat("f", 120)
	got := sanitizeReportedContentHash("  " + dirty + "  ")

	if len(got) > maxReportedContentHashBytes {
		t.Fatalf("sanitized length = %d, want <= %d (column width)", len(got), maxReportedContentHashBytes)
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("sanitized value still carries control character %q: %q", r, got)
		}
	}
	if strings.HasPrefix(got, " ") || strings.HasSuffix(got, " ") {
		t.Fatalf("sanitized value keeps surrounding whitespace: %q", got)
	}
}

// R8(AC9):消毒必须发生在落库路径上,不只是函数可用。
func TestHelloObservationPersistsSanitizedContentHash(t *testing.T) {
	gdb := openTestDB(t)
	server := NewTransportServer(TransportDeps{DB: gdb})

	server.recordHelloObservation(1, 7,
		"dead\x00beef"+strings.Repeat("a", 200), "us-east-1\x1b")

	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatalf("load node state: %v", err)
	}
	if len(state.ContentHash) > maxReportedContentHashBytes {
		t.Fatalf("persisted content_hash length = %d, want <= %d",
			len(state.ContentHash), maxReportedContentHashBytes)
	}
	if strings.ContainsRune(state.ContentHash, '\x00') {
		t.Fatalf("persisted content_hash still has a control character: %q", state.ContentHash)
	}
	if strings.ContainsRune(state.Region, '\x1b') {
		t.Fatalf("persisted region still has a control character: %q", state.Region)
	}
}

// R5:落库失败按存储类错误处理——一次失败不拆连接,但也不能假装落库成功地把节流
// 时间戳留在"已落库"状态之外。这里确认 touchHeartbeat 会把错误如实返回给调用方。
func TestTouchHeartbeatReportsStorageErrors(t *testing.T) {
	gdb := openTestDB(t)
	server := NewTransportServer(TransportDeps{DB: gdb})

	// 正常路径返回 nil。
	if err := server.touchHeartbeat(1, controlproto.HeartbeatPayload{AppliedVersion: 3}); err != nil {
		t.Fatalf("touchHeartbeat on a healthy DB = %v, want nil", err)
	}

	// 注入一个必然失败的写:关掉底层连接池。
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := server.touchHeartbeat(1, controlproto.HeartbeatPayload{AppliedVersion: 4}); err == nil {
		t.Fatal("touchHeartbeat swallowed a storage error; the caller can no longer grade it")
	}
}
