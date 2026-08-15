package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func i64p(v int64) *int64 { return &v }

// 旧库(node_states 没有遥测列)必须通过 additive migration 无损升级。
func TestMigrateAddsTelemetryColumnsToLegacySchema(t *testing.T) {
	gdb := openTestDB(t)
	// 先把新增列整体回退成旧 schema,模拟升级前的旧库。
	if err := gdb.Exec(`DROP TABLE node_states`).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`CREATE TABLE node_states (
		id integer PRIMARY KEY AUTOINCREMENT,
		node_id integer NOT NULL,
		online numeric NOT NULL DEFAULT 0,
		applied_version integer NOT NULL DEFAULT 0,
		sync_state text NOT NULL DEFAULT 'waiting',
		content_hash text,
		last_error text,
		region text,
		last_heartbeat datetime,
		updated_at datetime)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`INSERT INTO node_states (node_id, online, sync_state) VALUES (9, 1, 'synced')`).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(gdb); err != nil {
		t.Fatalf("upgrade legacy schema: %v", err)
	}
	for _, column := range []string{"used_bytes_total", "object_count", "telemetry_observed_at", "telemetry_valid"} {
		if !gdb.Migrator().HasColumn(&NodeState{}, column) {
			t.Fatalf("expected column %q after migration", column)
		}
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 9).First(&state).Error; err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if !state.Online || state.SyncState != SyncStateSynced {
		t.Fatalf("legacy row corrupted: %+v", state)
	}
	if state.UsedBytesTotal != nil || state.ObjectCount != nil || state.TelemetryObservedAt != nil {
		t.Fatalf("legacy row must keep telemetry columns NULL, got %+v", state)
	}
	if state.TelemetryValid {
		t.Fatal("legacy row must start telemetry-invalid")
	}
}

func touchHeartbeatPayload(applied int64, usedBytes, objects *int64, observedAt string) controlproto.HeartbeatPayload {
	return controlproto.HeartbeatPayload{
		AppliedVersion: applied,
		UsedBytesTotal: usedBytes,
		ObjectCount:    objects,
		ObservedAt:     observedAt,
	}
}

// 完整心跳:持久化实际值与节点观测时间;合法的 0/0 不被写成 NULL。
func TestTouchHeartbeatPersistsCompleteTelemetry(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	nodeID := createDashboardNode(t, api, "telemetry-node")
	observed := "2026-08-15T12:00:00Z"

	api.transport.touchHeartbeat(nodeID, touchHeartbeatPayload(3, i64p(1234), i64p(7), observed))

	var state NodeState
	if err := api.db.Where("node_id = ?", nodeID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.UsedBytesTotal == nil || *state.UsedBytesTotal != 1234 {
		t.Fatalf("used_bytes_total = %v, want 1234", state.UsedBytesTotal)
	}
	if state.ObjectCount == nil || *state.ObjectCount != 7 {
		t.Fatalf("object_count = %v, want 7", state.ObjectCount)
	}
	if state.TelemetryObservedAt == nil || !state.TelemetryObservedAt.UTC().Equal(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("observed_at = %v", state.TelemetryObservedAt)
	}
	if !state.TelemetryValid {
		t.Fatal("complete snapshot must mark telemetry valid")
	}

	// 合法的 0 字节/0 对象:仍然是有效观测,且保持显式 0。
	api.transport.touchHeartbeat(nodeID, touchHeartbeatPayload(4, i64p(0), i64p(0), observed))
	state = NodeState{}
	if err := api.db.Where("node_id = ?", nodeID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.UsedBytesTotal == nil || *state.UsedBytesTotal != 0 || state.ObjectCount == nil || *state.ObjectCount != 0 {
		t.Fatalf("explicit zero corrupted: %+v", state)
	}
	if !state.TelemetryValid {
		t.Fatal("explicit zero snapshot must stay valid")
	}
}

// 旧版心跳:遥测失效但旧值保留(供排查),不能伪装成当前观测,也不写 0。
func TestTouchHeartbeatLegacyMarksUnavailable(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	nodeID := createDashboardNode(t, api, "legacy-node")

	api.transport.touchHeartbeat(nodeID, touchHeartbeatPayload(1, i64p(500), i64p(2), "2026-08-15T12:00:00Z"))
	api.transport.touchHeartbeat(nodeID, touchHeartbeatPayload(2, nil, nil, ""))

	var state NodeState
	if err := api.db.Where("node_id = ?", nodeID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.TelemetryValid {
		t.Fatal("legacy heartbeat must invalidate telemetry")
	}
	if state.UsedBytesTotal == nil || *state.UsedBytesTotal != 500 || state.ObjectCount == nil || *state.ObjectCount != 2 {
		t.Fatalf("stale snapshot must stay inspectable, got %+v", state)
	}
	if !state.Online {
		t.Fatal("liveness must still update on legacy heartbeats")
	}
}

// 字段不完整的心跳按不可用处理,且不写任何遥测列。
func TestTouchHeartbeatPartialTelemetryIgnored(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	nodeID := createDashboardNode(t, api, "partial-node")

	// 只有 bytes,没有 count/observed_at。
	api.transport.touchHeartbeat(nodeID, touchHeartbeatPayload(1, i64p(500), nil, ""))
	var state NodeState
	if err := api.db.Where("node_id = ?", nodeID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.TelemetryValid || state.UsedBytesTotal != nil || state.ObjectCount != nil || state.TelemetryObservedAt != nil {
		t.Fatalf("partial telemetry must be ignored, got %+v", state)
	}

	// 非法 observed_at 同样不可用。
	node2 := createDashboardNode(t, api, "bad-ts-node")
	api.transport.touchHeartbeat(node2, touchHeartbeatPayload(1, i64p(5), i64p(1), "not-a-timestamp"))
	state = NodeState{}
	if err := api.db.Where("node_id = ?", node2).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.TelemetryValid || state.UsedBytesTotal != nil {
		t.Fatalf("malformed observed_at must be ignored, got %+v", state)
	}
}

// 汇总口径:只有"有效且未过期"的观测计入总量;缺失/过期节点单独计数且
// 不贡献 0;退役节点完全排除;阈值来自注入的配置而非硬编码。
func TestDashboardTelemetryAggregation(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	// 阈值收紧到 30 秒,证明汇总真的使用注入值。
	api.SetTelemetryExpiry(30 * time.Second)
	now := nowUTC()
	fresh := now.Add(-10 * time.Second)
	staleTime := now.Add(-10 * time.Minute)

	validNode := createDashboardNode(t, api, "valid")
	zeroNode := createDashboardNode(t, api, "zero")
	missingNode := createDashboardNode(t, api, "missing")
	staleNode := createDashboardNode(t, api, "stale")
	retiredNode := createDashboardNode(t, api, "retired")

	createState := func(nodeID uint, used, objects *int64, observed *time.Time, valid bool) {
		t.Helper()
		if err := api.db.Create(&NodeState{
			NodeID: nodeID, Online: true, SyncState: SyncStateSynced,
			UsedBytesTotal: used, ObjectCount: objects, TelemetryObservedAt: observed,
			TelemetryValid: valid, LastHeartbeat: &now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	createState(validNode, i64p(1000), i64p(10), &fresh, true)
	createState(zeroNode, i64p(0), i64p(0), &fresh, true)
	createState(missingNode, nil, nil, nil, false)
	// 旧值仍在但已被旧版心跳覆盖成 invalid:按缺失计。
	createState(staleNode, i64p(777), i64p(3), &staleTime, false)
	if err := api.db.Model(&Node{}).Where("id = ?", retiredNode).Update("status", NodeStatusRetired).Error; err != nil {
		t.Fatal(err)
	}
	// 退役节点的有效观测也不得进入遥测(与健康口径一致)。
	if err := api.db.Create(&NodeState{
		NodeID: retiredNode, Online: true, SyncState: SyncStateSynced,
		UsedBytesTotal: i64p(999999), ObjectCount: i64p(999), TelemetryObservedAt: &fresh,
		TelemetryValid: true, LastHeartbeat: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	body := serveDashboard(t, api, http.MethodGet)
	tele := body.Telemetry
	if tele.UsedBytesTotal != 1000 || tele.ObjectCount != 10 {
		t.Fatalf("totals = %d bytes / %d objects, want 1000 / 10", tele.UsedBytesTotal, tele.ObjectCount)
	}
	if tele.ValidNodes != 2 || tele.MissingNodes != 2 {
		t.Fatalf("valid=%d missing=%d stale=%d, want valid=2 missing=2 stale=0",
			tele.ValidNodes, tele.MissingNodes, tele.StaleNodes)
	}
	if tele.StaleNodes != 0 {
		t.Fatalf("invalid snapshot must count as missing, not stale: %d", tele.StaleNodes)
	}
	if len(tele.Nodes) != 4 {
		t.Fatalf("telemetry rows = %d, want 4 non-retired nodes", len(tele.Nodes))
	}
	statuses := map[uint]string{}
	for _, entry := range tele.Nodes {
		statuses[entry.NodeID] = entry.Status
	}
	if statuses[validNode] != "valid" || statuses[zeroNode] != "valid" ||
		statuses[missingNode] != "missing" || statuses[staleNode] != "missing" {
		t.Fatalf("statuses = %v", statuses)
	}

	// 过期路径:把 staleNode 改回 valid 但观测时间超阈值。
	if err := api.db.Model(&NodeState{}).Where("node_id = ?", staleNode).
		Updates(map[string]any{"telemetry_valid": true}).Error; err != nil {
		t.Fatal(err)
	}
	body = serveDashboard(t, api, http.MethodGet)
	if body.Telemetry.StaleNodes != 1 || body.Telemetry.ValidNodes != 2 {
		t.Fatalf("stale=%d valid=%d, want stale=1 valid=2", body.Telemetry.StaleNodes, body.Telemetry.ValidNodes)
	}
	if body.Telemetry.UsedBytesTotal != 1000 {
		t.Fatalf("stale node leaked into totals: %d", body.Telemetry.UsedBytesTotal)
	}
}

// 缺失遥测在 JSON 里必须是显式 null,前端才能区分"未上报"与 0。
func TestDashboardTelemetryMissingSerializesAsNull(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	nodeID := createDashboardNode(t, api, "null-node")
	if err := api.db.Create(&NodeState{NodeID: nodeID, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	api.DashboardSummary(rw, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/summary", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("summary = %d", rw.Code)
	}
	var raw struct {
		Telemetry struct {
			UsedBytesTotal int64 `json:"used_bytes_total"`
			Nodes          []struct {
				NodeID    uint   `json:"node_id"`
				UsedBytes *int64 `json:"used_bytes"`
				Status    string `json:"status"`
			} `json:"nodes"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Telemetry.UsedBytesTotal != 0 || len(raw.Telemetry.Nodes) != 1 ||
		raw.Telemetry.Nodes[0].NodeID != nodeID || raw.Telemetry.Nodes[0].UsedBytes != nil ||
		raw.Telemetry.Nodes[0].Status != "missing" {
		t.Fatalf("unexpected telemetry payload: %s", rw.Body.String())
	}
}

// 非 GET 一律 405(与既有汇总 handler 的方法口径一致)。
func TestDashboardSummaryRejectsNonGET(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	rw := httptest.NewRecorder()
	api.DashboardSummary(rw, httptest.NewRequest(http.MethodPost, "/api/admin/dashboard/summary", strings.NewReader("{}")))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST summary = %d, want 405", rw.Code)
	}
}
