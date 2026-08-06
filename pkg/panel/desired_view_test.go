package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// TestGetDesiredState_ReturnsPublishedView 覆盖 prd Acceptance Criteria:
// GET /api/admin/nodes/{id}/desired-state 返回已发布快照的脱敏视图,
// 含 version 与 content_hash,字段齐全。
func TestGetDesiredState_ReturnsPublishedView(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	// 创建凭证 + 桶 + webhook + 限流,让快照内容齐全
	serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app","quota_bytes":100}`)
	serve(api, http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"mybucket","acl":"public-read"}`)
	serve(api, http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"http://hook.example","events":["ObjectCreated","ObjectDeleted"],"enabled":true}`)
	serve(api, http.MethodPut, "/api/admin/nodes/1/rate-limit", `{"anonymous_rps":5,"anonymous_burst":10,"trust_forwarded":false}`)
	// 发布
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var pub publishResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &pub)

	// GET 脱敏视图
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("get desired state status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var view PublishedSnapshotView
	if err := json.Unmarshal(rw.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode view: %v; body=%s", err, rw.Body.String())
	}
	if !view.Published {
		t.Fatalf("view.Published = false, want true")
	}
	if view.Version != pub.Version {
		t.Fatalf("version = %d, want %d", view.Version, pub.Version)
	}
	if view.ContentHash != pub.ContentHash || view.ContentHash == "" {
		t.Fatalf("content_hash = %q, want %q", view.ContentHash, pub.ContentHash)
	}
	if view.SchemaVersion != desiredSnapshotSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", view.SchemaVersion, desiredSnapshotSchemaVersion)
	}
	if len(view.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(view.Credentials))
	}
	if view.Credentials[0].AccessKey == "" {
		t.Fatal("credential access_key empty")
	}
	if view.Credentials[0].QuotaBytes != 100 {
		t.Fatalf("quota_bytes = %d, want 100", view.Credentials[0].QuotaBytes)
	}
	if len(view.Buckets) != 1 || view.Buckets[0].ACL != "public-read" {
		t.Fatalf("buckets = %+v", view.Buckets)
	}
	if len(view.Webhooks) != 1 {
		t.Fatalf("webhooks = %d, want 1", len(view.Webhooks))
	}
	// webhook events 必须已拆成数组
	if len(view.Webhooks[0].Events) != 2 {
		t.Fatalf("webhook events = %v, want 2 items", view.Webhooks[0].Events)
	}
	if view.RateLimit == nil || view.RateLimit.AnonymousRPS != 5 {
		t.Fatalf("rate_limit = %+v", view.RateLimit)
	}
}

// TestGetDesiredState_NoSecretLeakage 是脱敏红线的自动化守卫(prd R2/红线):
// 构造含真实密文(SecretKeyCipher)与明文 secret 的已发布快照,
// 断言响应体字符串既不含明文 secret,也不含密文串。
func TestGetDesiredState_NoSecretLeakage(t *testing.T) {
	api, cipher := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`)
	var created credentialResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &created)
	plaintextSecret := created.SecretKey // 仅此一处出现的明文
	if plaintextSecret == "" {
		t.Fatal("create must return plaintext secret")
	}
	// 取出存储的密文,用于断言响应也不含密文串
	var credRow NodeCredential
	if err := api.db.Where("node_id = ?", 1).First(&credRow).Error; err != nil {
		t.Fatalf("load credential: %v", err)
	}
	ciphertext := credRow.SecretKeyCipher
	if ciphertext == "" {
		t.Fatal("stored ciphertext empty")
	}
	// 确保密文确实能解回明文(证明它就是该明文的密文)
	dec, err := cipher.Decrypt(ciphertext)
	if err != nil || dec != plaintextSecret {
		t.Fatalf("cipher roundtrip: dec=%q err=%v", dec, err)
	}

	// 发布(把含密文的快照固化)
	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("publish status = %d", rw.Code)
	}

	// GET:响应体不得含明文 secret,也不得含密文串
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if strings.Contains(body, plaintextSecret) {
		t.Fatalf("response leaked plaintext secret %q: %s", plaintextSecret, body)
	}
	if strings.Contains(body, ciphertext) {
		t.Fatalf("response leaked ciphertext: %s", body)
	}
	// 字段名层面也不得出现 secret_key / secret_key_cipher
	if strings.Contains(body, "secret_key") || strings.Contains(body, "SecretKey") {
		t.Fatalf("response contains secret key field name: %s", body)
	}
}

// TestGetDesiredState_NoPublishedReturnsEmptyState200 验证无发布行时返回
// 200 空态(published=false, 切片为 [] 非 null),而非 404/500。
func TestGetDesiredState_NoPublishedReturnsEmptyState200(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)

	rw := serve(api, http.MethodGet, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	var view PublishedSnapshotView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Published {
		t.Fatalf("published = true, want false")
	}
	// 切片必须是 [] 而非 null:JSON 里应出现 "credentials":[]
	if !strings.Contains(body, `"credentials":[]`) {
		t.Fatalf("credentials must be [] not null: %s", body)
	}
	if !strings.Contains(body, `"buckets":[]`) {
		t.Fatalf("buckets must be [] not null: %s", body)
	}
	if !strings.Contains(body, `"webhooks":[]`) {
		t.Fatalf("webhooks must be [] not null: %s", body)
	}
}

// TestGetDesiredState_LegacySnapshotMarksRepublishNeeded 验证旧格式快照
// 返回 republish_needed=true(非 500),且不回填草稿。
func TestGetDesiredState_LegacySnapshotMarksRepublishNeeded(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	// 直接写一个无 schema_version 的旧格式快照
	legacy := controlproto.DesiredState{}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Create(&DesiredConfig{NodeID: 1, Version: 2, ContentJSON: string(raw), ContentHash: legacy.ContentHash()}).Error; err != nil {
		t.Fatal(err)
	}

	rw := serve(api, http.MethodGet, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	var view PublishedSnapshotView
	if err := json.Unmarshal(rw.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.RepublishNeeded {
		t.Fatalf("republish_needed = false, want true; body=%s", rw.Body.String())
	}
	if !view.Published {
		t.Fatalf("published = false, want true (a config row exists)")
	}
	// 旧格式内容为空切片,不回填草稿
	if len(view.Credentials) != 0 || len(view.Buckets) != 0 || len(view.Webhooks) != 0 {
		t.Fatalf("legacy view must have empty content, got %+v", view)
	}
}

// TestGetDesiredState_UnknownNodeReturns404 验证未知 node id 返回 404。
func TestGetDesiredState_UnknownNodeReturns404(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	rw := serve(api, http.MethodGet, "/api/admin/nodes/999/desired-state", "")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rw.Code, rw.Body.String())
	}
}

// TestGetDesiredState_PostAndPushUnchanged 验证既有 POST 发布与 POST /push
// 行为零变化(prd R3/红线:publish/push 测试零修改通过)。
func TestGetDesiredState_PostAndPushUnchanged(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`)
	// POST 发布仍然 200
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("publish status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	var pub publishResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode publish: %v", err)
	}
	if pub.Version != 1 || pub.ContentHash == "" {
		t.Fatalf("publish response = %+v", pub)
	}
	// 节点离线时 POST /push 返回 409(既有约定)
	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state/push", "")
	if rw.Code != http.StatusConflict {
		t.Fatalf("push offline status = %d, want 409; body=%s", rw.Code, rw.Body.String())
	}
}

// TestGetDesiredState_NonGetPostMethodReturns404 验证非 GET/POST 方法
// 走既有的 404 约定(与同文件 desiredStateRoute 其它未覆盖方法一致)。
func TestGetDesiredState_NonGetPostMethodReturns404(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	rw := serve(api, http.MethodDelete, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("delete status = %d, want 404; body=%s", rw.Code, rw.Body.String())
	}
}
