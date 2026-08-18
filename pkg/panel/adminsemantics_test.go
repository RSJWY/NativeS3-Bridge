package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// R9 的语义修正回归:管理 API 在"节点不存在 / 路径多余段 / 快照校验失败 / 节点已退役"
// 这几类错误输入下的响应码与响应体必须是单一且正确的。

// R9.1:updateNode/retireNode 末尾重新读取节点时若失败,不能在已写出的错误响应之后
// 再写一段 200。用 Query 回调把"第二次查 nodes"注入为失败来复现该交错。
func TestUpdateNodeWritesSingleResponseWhenReloadFails(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)

	var nodeQueries int
	if err := api.db.Callback().Query().After("gorm:query").Register("test:fail_second_node_query", func(tx *gorm.DB) {
		if tx.Statement.Table != "nodes" {
			return
		}
		nodeQueries++
		// 第 1 次是 handler 入口的 loadNode(必须成功),第 2 次是更新后的重新读取。
		if nodeQueries == 2 {
			tx.AddError(errors.New("injected db failure"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	rw := serve(api, http.MethodPatch, "/api/admin/nodes/1", `{"display_name":"renamed"}`)
	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from the failed reload", rw.Code)
	}
	// 双写的症状是响应体里连着两段 JSON,整体无法解析为单个对象。
	var payload map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body is not a single JSON object (double write): %q", rw.Body.String())
	}
	if _, leaked := payload["display_name"]; leaked {
		t.Fatalf("error response must not carry a node payload: %q", rw.Body.String())
	}
}

// R9.2:certs 子路由必须和 credentials/buckets/tasks 一样,对不存在的节点返回 404,
// 而不是对着空表返回 200。
func TestCertsRouteReturnsNotFoundForUnknownNode(t *testing.T) {
	api, _ := newTestAdminAPI(t)

	rw := serve(api, http.MethodGet, "/api/admin/nodes/999/certs", "")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("list certs for unknown node = %d, want 404, body=%s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPost, "/api/admin/nodes/999/certs/revoke", "")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("revoke certs for unknown node = %d, want 404, body=%s", rw.Code, rw.Body.String())
	}
}

// R9.3:tokens 与 certs/revoke 都没有更深的子资源,多余路径段必须 404,不能被当成
// 合法的签发/撤销请求。
func TestAdminNodeRoutesRejectExtraPathSegments(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)

	cases := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/api/admin/nodes/1/tokens/extra"},
		{http.MethodPost, "/api/admin/nodes/1/certs/revoke/extra"},
	}
	for _, tc := range cases {
		rw := serve(api, tc.method, tc.target, "")
		if rw.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404, body=%s", tc.method, tc.target, rw.Code, rw.Body.String())
		}
	}

	// 对照:去掉多余段后仍是正常请求,证明上面的 404 来自段数校验而不是整条路由失效。
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/tokens", "")
	if rw.Code != http.StatusCreated && rw.Code != http.StatusOK {
		t.Fatalf("issue token = %d, want success, body=%s", rw.Code, rw.Body.String())
	}
}

// R9.4:已发布快照的完整性校验失败是"需要管理员重新发布"的冲突,不是服务端故障。
func TestPushDesiredStateMapsHashMismatchToConflict(t *testing.T) {
	api, cipher := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)

	authority := NewDesiredStateAuthority(api.db, cipher)
	if _, _, err := authority.Publish(1, "admin"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// 篡改落库的 content_hash,使 BuildPushable 走 fail-closed 分支。
	if err := api.db.Model(&DesiredConfig{}).Where("node_id = ?", 1).
		Update("content_hash", strings.Repeat("0", 64)).Error; err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}
	// 节点必须在线且具备权威配置能力,否则会先命中 offline / capability 分支。
	api.hub.Register(1, &AgentConn{
		NodeID:       1,
		Capabilities: []string{controlproto.CapabilityAuthoritativeConfigV1},
	})

	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state/push", "")
	if rw.Code != http.StatusConflict {
		t.Fatalf("push with corrupted snapshot = %d, want 409, body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "publish again") {
		t.Fatalf("response should carry the republish hint, got %s", rw.Body.String())
	}
}

// R9.5:退役是不可逆终态,子资源不再接受写入(否则产生永远下发不出去的草稿),
// 但只读视图必须保留供审计追溯。
func TestRetiredNodeRejectsSubresourceWritesButStaysReadable(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	if rw := serve(api, http.MethodDelete, "/api/admin/nodes/1", ""); rw.Code != http.StatusOK {
		t.Fatalf("retire = %d, body=%s", rw.Code, rw.Body.String())
	}

	writes := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`},
		{http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"bucket-a"}`},
		{http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"https://example.com/hook"}`},
		{http.MethodPut, "/api/admin/nodes/1/rate-limit", `{"anonymous_rps":10,"anonymous_burst":20}`},
	}
	for _, tc := range writes {
		rw := serve(api, tc.method, tc.target, tc.body)
		if rw.Code != http.StatusConflict {
			t.Fatalf("%s %s on retired node = %d, want 409, body=%s", tc.method, tc.target, rw.Code, rw.Body.String())
		}
	}

	reads := []string{
		"/api/admin/nodes/1/credentials",
		"/api/admin/nodes/1/buckets",
		"/api/admin/nodes/1/webhooks",
		"/api/admin/nodes/1/rate-limit",
	}
	for _, target := range reads {
		rw := serve(api, http.MethodGet, target, "")
		if rw.Code != http.StatusOK {
			t.Fatalf("GET %s on retired node = %d, want 200 (audit visibility), body=%s", target, rw.Code, rw.Body.String())
		}
	}
}
