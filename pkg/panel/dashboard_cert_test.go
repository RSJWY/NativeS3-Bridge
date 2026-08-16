package panel

import (
	"net/http"
	"testing"
	"time"
)

// seedNodeCert 写一行证书并返回它。
func seedNodeCert(t *testing.T, api *AdminAPI, nodeID uint, fingerprint string, notBefore, notAfter time.Time, revoked bool) NodeCert {
	t.Helper()
	cert := NodeCert{
		NodeID: nodeID, Fingerprint: fingerprint, Serial: fingerprint,
		NotBefore: notBefore, NotAfter: notAfter, Revoked: revoked,
	}
	if err := api.db.Create(&cert).Error; err != nil {
		t.Fatal(err)
	}
	return cert
}

// AC9/AC10:dashboard 返回可区分的临期/已过期计数,已过期节点进
// AttentionNodes 且 severity=cert_expired 排在 sync_failed 之前(rank 5>4)。
func TestDashboardSummaryCertExpiryAggregation(t *testing.T) {
	api, _ := newTestAdminAPI(t)

	expiredNode := createDashboardNode(t, api, "cert-expired")
	expiringNode := createDashboardNode(t, api, "cert-expiring")
	healthyNode := createDashboardNode(t, api, "cert-healthy")
	revokedOnlyNode := createDashboardNode(t, api, "cert-revoked-only")

	// 已过期:NotAfter 在过去。为了让 severity 稳定,再让该节点同步失败,
	// 验证 cert_expired(5) 压过 sync_failed(4)。
	if err := api.db.Create(&NodeState{NodeID: expiredNode, Online: true, SyncState: SyncStateFailed, LastError: "x"}).Error; err != nil {
		t.Fatal(err)
	}
	api.hub.Register(expiredNode, &AgentConn{NodeID: expiredNode})
	seedNodeCert(t, api, expiredNode, "fp-expired",
		time.Now().Add(-90*24*time.Hour), time.Now().Add(-24*time.Hour), false)

	// 临期:TTL 30 天,已过 22 天,剩余 8 天 < 10 天阈值。节点在线无其它异常,
	// 若没有证书维度它根本不会进关注列表 -- 这验证 cert_expiring 也能触发关注。
	api.hub.Register(expiringNode, &AgentConn{NodeID: expiringNode})
	if err := api.db.Create(&NodeState{NodeID: expiringNode, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	version, hash, err := api.desired.Publish(expiringNode, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Model(&NodeState{}).Where("node_id = ?", expiringNode).
		Updates(map[string]any{"applied_version": version, "content_hash": hash}).Error; err != nil {
		t.Fatal(err)
	}
	seedNodeCert(t, api, expiringNode, "fp-expiring",
		time.Now().Add(-22*24*time.Hour), time.Now().Add(8*24*time.Hour), false)

	// 健康:TTL 90 天,剩余 80 天。
	api.hub.Register(healthyNode, &AgentConn{NodeID: healthyNode})
	if err := api.db.Create(&NodeState{NodeID: healthyNode, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	hv, hh, err := api.desired.Publish(healthyNode, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Model(&NodeState{}).Where("node_id = ?", healthyNode).
		Updates(map[string]any{"applied_version": hv, "content_hash": hh}).Error; err != nil {
		t.Fatal(err)
	}
	seedNodeCert(t, api, healthyNode, "fp-healthy",
		time.Now().Add(-10*24*time.Hour), time.Now().Add(80*24*time.Hour), false)

	// 只有已吊销证书(且已过期):不计入任何聚合,也不进关注列表。
	api.hub.Register(revokedOnlyNode, &AgentConn{NodeID: revokedOnlyNode})
	if err := api.db.Create(&NodeState{NodeID: revokedOnlyNode, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	rv, rh, err := api.desired.Publish(revokedOnlyNode, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Model(&NodeState{}).Where("node_id = ?", revokedOnlyNode).
		Updates(map[string]any{"applied_version": rv, "content_hash": rh}).Error; err != nil {
		t.Fatal(err)
	}
	seedNodeCert(t, api, revokedOnlyNode, "fp-revoked",
		time.Now().Add(-90*24*time.Hour), time.Now().Add(-24*time.Hour), true)

	body := serveDashboard(t, api, http.MethodGet)
	if body.Certs.ExpiredNodes != 1 || body.Certs.ExpiringNodes != 1 {
		t.Fatalf("cert aggregates = %+v, want expired=1 expiring=1", body.Certs)
	}
	if body.Totals.Attention != 2 {
		t.Fatalf("attention = %d, want 2 (expired + expiring only)", body.Totals.Attention)
	}
	if len(body.AttentionNodes) != 2 {
		t.Fatalf("attention nodes = %+v", body.AttentionNodes)
	}
	// severityRank:cert_expired(5) 必须排在最前。
	if body.AttentionNodes[0].ID != expiredNode || body.AttentionNodes[0].Severity != severityCertExpired {
		t.Fatalf("top attention node = %+v, want node %d with cert_expired", body.AttentionNodes[0], expiredNode)
	}
	if body.AttentionNodes[1].ID != expiringNode || body.AttentionNodes[1].Severity != severityCertExpiring {
		t.Fatalf("second attention node = %+v, want node %d with cert_expiring", body.AttentionNodes[1], expiringNode)
	}
}

// 当前证书口径:同一节点两张未吊销证书时,取 NotAfter 最大的一张(design §3.2)。
// 旧证书已临期但新证书健康 -> 节点应判为健康,不计入临期。
func TestDashboardCurrentCertPicksLatestNotAfter(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	node := createDashboardNode(t, api, "dual-cert")
	api.hub.Register(node, &AgentConn{NodeID: node})
	if err := api.db.Create(&NodeState{NodeID: node, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	version, hash, err := api.desired.Publish(node, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.db.Model(&NodeState{}).Where("node_id = ?", node).
		Updates(map[string]any{"applied_version": version, "content_hash": hash}).Error; err != nil {
		t.Fatal(err)
	}
	// 旧证书:剩余 5 天(临期)。新证书:剩余 80 天(健康)。
	seedNodeCert(t, api, node, "fp-old",
		time.Now().Add(-85*24*time.Hour), time.Now().Add(5*24*time.Hour), false)
	seedNodeCert(t, api, node, "fp-new",
		time.Now().Add(-10*24*time.Hour), time.Now().Add(80*24*time.Hour), false)

	body := serveDashboard(t, api, http.MethodGet)
	if body.Certs.ExpiringNodes != 0 || body.Certs.ExpiredNodes != 0 {
		t.Fatalf("cert aggregates = %+v, want all zero", body.Certs)
	}
	if body.Totals.Attention != 0 || len(body.AttentionNodes) != 0 {
		t.Fatalf("node with healthy current cert must not be flagged: %+v", body.AttentionNodes)
	}
}
