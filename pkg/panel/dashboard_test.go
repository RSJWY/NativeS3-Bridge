package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// serveDashboard 调用仪表盘汇总 handler 并解码响应。
func serveDashboard(t *testing.T, api *AdminAPI, method string) dashboardSummaryResponse {
	t.Helper()
	rw := httptest.NewRecorder()
	api.DashboardSummary(rw, httptest.NewRequest(method, "/api/admin/dashboard/summary", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("dashboard summary status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var body dashboardSummaryResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode dashboard summary: %v", err)
	}
	return body
}

func createDashboardNode(t *testing.T, api *AdminAPI, name string) uint {
	t.Helper()
	rw := serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"`+name+`"}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create node %s = %d %s", name, rw.Code, rw.Body.String())
	}
	var node nodeResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	return node.ID
}

func TestDashboardSummaryAggregatesLifecycleSyncAndAttention(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	older := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)

	// 节点1:在线、已发布且已同步,健康节点。
	healthy := createDashboardNode(t, api, "healthy")
	version, hash, err := api.desired.Publish(healthy, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&NodeState{
		NodeID: healthy, Online: true, AppliedVersion: version, ContentHash: hash, SyncState: SyncStateSynced,
		Region: "cn-shanghai", LastHeartbeat: &older,
	}).Error; err != nil {
		t.Fatal(err)
	}
	api.hub.Register(healthy, &AgentConn{NodeID: healthy})

	// 节点2:离线且同步失败,保留错误证据和旧心跳。
	failed := createDashboardNode(t, api, "failed")
	failedError := "apply rejected: version regression"
	if err := api.db.Create(&NodeState{
		NodeID: failed, SyncState: SyncStateFailed, LastError: failedError, LastHeartbeat: &older,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 节点3:在线但配置漂移。
	drift := createDashboardNode(t, api, "drift")
	if err := api.db.Create(&NodeState{NodeID: drift, Online: true, SyncState: SyncStateDrift, LastHeartbeat: &older}).Error; err != nil {
		t.Fatal(err)
	}
	api.hub.Register(drift, &AgentConn{NodeID: drift})

	// 节点4:已退役,但 Hub 残留在线连接,不得被计为健康在线节点。
	retired := createDashboardNode(t, api, "retired")
	if err := api.db.Model(&Node{}).Where("id = ?", retired).Update("status", NodeStatusRetired).Error; err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&NodeState{NodeID: retired, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	api.hub.Register(retired, &AgentConn{NodeID: retired})

	// 节点5:刚创建,从未发布也没有 NodeState:draft_dirty 待发布,同步状态未知。
	fresh := createDashboardNode(t, api, "fresh")

	// 节点7:同步已成功但当前离线:仅离线一项也必须进入关注列表。
	offlineHealthy := createDashboardNode(t, api, "offline-healthy")
	offlineVersion, offlineHash, err := api.desired.Publish(offlineHealthy, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&NodeState{
		NodeID: offlineHealthy, AppliedVersion: offlineVersion, ContentHash: offlineHash, SyncState: SyncStateSynced,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 节点6:遗留快照需要重新发布,nodeToResponse 会标记为 failed + publish_required。
	legacy := createDashboardNode(t, api, "legacy")
	legacyState := controlproto.DesiredState{}
	raw, err := json.Marshal(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&DesiredConfig{NodeID: legacy, Version: 2, ContentJSON: string(raw), ContentHash: legacyState.ContentHash()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&NodeState{NodeID: legacy, AppliedVersion: 2, ContentHash: legacyState.ContentHash(), SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}

	body := serveDashboard(t, api, http.MethodGet)
	if body.Totals != (dashboardTotals{Nodes: 7, Online: 2, Offline: 4, Retired: 1, Attention: 5}) {
		t.Fatalf("totals = %+v", body.Totals)
	}
	if body.Health != (dashboardHealth{Synced: 2, Waiting: 0, Failed: 2, Drift: 1, Unknown: 1}) {
		t.Fatalf("health = %+v", body.Health)
	}
	// 严重性降序:sync_failed(有旧心跳优先,缺失心跳最后)> drift > pending;
	// 待发布节点即使 sync_state 为空也必须进入列表。
	gotIDs := make([]uint, 0, len(body.AttentionNodes))
	for _, item := range body.AttentionNodes {
		gotIDs = append(gotIDs, item.ID)
	}
	// offlineHealthy(6) 与 fresh(5) 同为 offline 级且都无心跳,按 ID 升序 fresh 在前。
	wantIDs := []uint{failed, legacy, drift, fresh, offlineHealthy}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("attention ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("attention order = %v, want %v", gotIDs, wantIDs)
		}
	}
	byID := make(map[uint]dashboardAttentionNode, len(body.AttentionNodes))
	for _, item := range body.AttentionNodes {
		byID[item.ID] = item
	}
	if byID[failed].Severity != severitySyncFailed || byID[legacy].Severity != severitySyncFailed {
		t.Fatalf("failed severities = %+v / %+v", byID[failed], byID[legacy])
	}
	if !byID[legacy].PublishRequired || byID[legacy].SyncState != SyncStateFailed {
		t.Fatalf("legacy attention node = %+v", byID[legacy])
	}
	if byID[drift].Severity != severityDrift {
		t.Fatalf("drift severity = %+v", byID[drift])
	}
	// 离线在严重性阶梯中高于待发布:fresh 节点同时离线和 draft_dirty,取 offline。
	if byID[offlineHealthy].Severity != severityOffline || byID[offlineHealthy].SyncState != SyncStateSynced {
		t.Fatalf("offline-healthy attention node = %+v", byID[offlineHealthy])
	}
	if byID[fresh].Severity != severityOffline || !byID[fresh].DraftDirty || byID[fresh].SyncState != "" {
		t.Fatalf("fresh attention node = %+v", byID[fresh])
	}
	if byID[fresh].Region != "" || byID[fresh].LastHeartbeat != nil || byID[fresh].LastError != "" {
		t.Fatalf("fresh node must carry empty observation fields, got %+v", byID[fresh])
	}
	if byID[failed].LastError != failedError {
		t.Fatalf("failed node lost last_error, got %+v", byID[failed])
	}
}

func TestDashboardSummaryWaitingNodeNeedsAttention(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	online := createDashboardNode(t, api, "online-waiting")
	offline := createDashboardNode(t, api, "offline-waiting")
	for _, id := range []uint{online, offline} {
		if _, _, err := api.desired.Publish(id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	// 两个节点都已发布但从未上报 NodeState:同步状态均为 waiting。
	// waiting 即待同步,按 design §3 口径必须进入关注列表:
	// 离线者取 offline 级,在线者取 pending 级。
	api.hub.Register(online, &AgentConn{NodeID: online})

	body := serveDashboard(t, api, http.MethodGet)
	if body.Totals != (dashboardTotals{Nodes: 2, Online: 1, Offline: 1, Retired: 0, Attention: 2}) {
		t.Fatalf("totals = %+v", body.Totals)
	}
	if body.Health != (dashboardHealth{Waiting: 2}) {
		t.Fatalf("health = %+v", body.Health)
	}
	if len(body.AttentionNodes) != 2 {
		t.Fatalf("attention nodes = %+v", body.AttentionNodes)
	}
	if body.AttentionNodes[0].ID != offline || body.AttentionNodes[0].Severity != severityOffline {
		t.Fatalf("offline waiting node = %+v", body.AttentionNodes[0])
	}
	if body.AttentionNodes[1].ID != online || body.AttentionNodes[1].Severity != severityPending {
		t.Fatalf("online waiting node = %+v", body.AttentionNodes[1])
	}
}

func TestDashboardSummaryOrderingTieBreaks(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	older := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

	// 三个同 severity 的离线待发布节点:心跳旧的在前,缺失心跳最后,并列按 ID 升序。
	noHeartbeatFirst := createDashboardNode(t, api, "a")
	newerHeartbeat := createDashboardNode(t, api, "b")
	olderHeartbeat := createDashboardNode(t, api, "c")
	// 再造一个心跳同样缺失、ID 更小的参照点。
	firstNil := createDashboardNode(t, api, "d")
	if err := api.db.Create(&NodeState{NodeID: newerHeartbeat, LastHeartbeat: &newer}).Error; err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&NodeState{NodeID: olderHeartbeat, LastHeartbeat: &older}).Error; err != nil {
		t.Fatal(err)
	}

	body := serveDashboard(t, api, http.MethodGet)
	gotIDs := make([]uint, 0, len(body.AttentionNodes))
	for _, item := range body.AttentionNodes {
		gotIDs = append(gotIDs, item.ID)
	}
	wantIDs := []uint{olderHeartbeat, newerHeartbeat, noHeartbeatFirst, firstNil}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("attention ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("attention order = %v, want %v", gotIDs, wantIDs)
		}
	}
}

func TestDashboardSummaryEmptyDatabase(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	body := serveDashboard(t, api, http.MethodGet)
	if body.Totals != (dashboardTotals{}) || body.Health != (dashboardHealth{}) {
		t.Fatalf("empty totals/health = %+v / %+v", body.Totals, body.Health)
	}
	if body.AttentionNodes == nil || len(body.AttentionNodes) != 0 {
		t.Fatalf("attention nodes must be empty slice, got %#v", body.AttentionNodes)
	}
	if body.GeneratedAt.IsZero() {
		t.Fatal("generated_at must be set")
	}
}

func TestDashboardSummaryRejectsNonGet(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rw := httptest.NewRecorder()
		api.DashboardSummary(rw, httptest.NewRequest(method, "/api/admin/dashboard/summary", nil))
		if rw.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, body=%s", method, rw.Code, rw.Body.String())
		}
	}
}
