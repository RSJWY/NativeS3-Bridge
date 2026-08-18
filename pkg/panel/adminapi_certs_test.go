package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// 四态判定的全部边界(AC2-AC4):吊销优先、恰好 NotAfter 时刻、
// 剩余恰好等于阈值、阈值随 TTL 等比缩放、过期天数为负。
func TestCertStatusBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		cert NodeCert
		want string
	}{
		{
			name: "revoked and also expired stays revoked",
			cert: NodeCert{
				Revoked:   true,
				NotBefore: now.Add(-90 * 24 * time.Hour),
				NotAfter:  now.Add(-24 * time.Hour),
			},
			want: certStatusRevoked,
		},
		{
			name: "exactly at NotAfter is expired",
			cert: NodeCert{
				NotBefore: now.Add(-90 * 24 * time.Hour),
				NotAfter:  now,
			},
			want: certStatusExpired,
		},
		{
			name: "remaining exactly at threshold is active",
			// 90 天 TTL -> 阈值 30 天;剩余恰好 30 天不是 < 阈值。
			cert: NodeCert{
				NotBefore: now.Add(-60 * 24 * time.Hour),
				NotAfter:  now.Add(30 * 24 * time.Hour),
			},
			want: certStatusActive,
		},
		{
			name: "remaining below threshold is expiring",
			// 90 天 TTL -> 阈值 30 天;剩余 29.9 天。
			cert: NodeCert{
				NotBefore: now.Add(-60 * 24 * time.Hour),
				NotAfter:  now.Add(30*24*time.Hour - time.Hour),
			},
			want: certStatusExpiring,
		},
		{
			name: "short TTL scales threshold proportionally",
			// 30 天 TTL -> 阈值 10 天;剩余 12 天 > 阈值 -> active。
			cert: NodeCert{
				NotBefore: now.Add(-18 * 24 * time.Hour),
				NotAfter:  now.Add(12 * 24 * time.Hour),
			},
			want: certStatusActive,
		},
		{
			name: "short TTL below proportional threshold is expiring",
			// 30 天 TTL -> 阈值 10 天;剩余 9 天 < 阈值。
			cert: NodeCert{
				NotBefore: now.Add(-21 * 24 * time.Hour),
				NotAfter:  now.Add(9 * 24 * time.Hour),
			},
			want: certStatusExpiring,
		},
		{
			name: "healthy cert is active",
			cert: NodeCert{
				NotBefore: now.Add(-10 * 24 * time.Hour),
				NotAfter:  now.Add(80 * 24 * time.Hour),
			},
			want: certStatusActive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := certStatus(tc.cert, now); got != tc.want {
				t.Fatalf("certStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// AC4 的机械验证:阈值必须随证书自身 TTL 变化,90 天证书阈值 30 天,
// 30 天证书阈值 10 天。
func TestCertExpiryThresholdProportional(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	if got := certExpiryThreshold(now, now.Add(90*24*time.Hour)); got != 30*24*time.Hour {
		t.Fatalf("90d cert threshold = %v, want 30d", got)
	}
	if got := certExpiryThreshold(now, now.Add(30*24*time.Hour)); got != 10*24*time.Hour {
		t.Fatalf("30d cert threshold = %v, want 10d", got)
	}
}

// AC3:已过期证书的剩余天数必须是负数(向下取整),不是未定义值。
func TestDaysUntilExpiryNegativeWhenExpired(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	if got := daysUntilExpiry(now.Add(-15*24*time.Hour), now); got != -15 {
		t.Fatalf("daysUntilExpiry(15d ago) = %d, want -15", got)
	}
	// 过期 14 天 23 小时:向下取整为 -15(第 15 天已在进行中)。
	if got := daysUntilExpiry(now.Add(-14*24*time.Hour-time.Hour), now); got != -15 {
		t.Fatalf("daysUntilExpiry(14d23h ago) = %d, want -15", got)
	}
	if got := daysUntilExpiry(now.Add(29*24*time.Hour+time.Hour), now); got != 29 {
		t.Fatalf("daysUntilExpiry(29d+1h) = %d, want 29", got)
	}
}

// AC1:GET /certs 返回 snake_case 字段,含剩余天数与状态字段。
func TestCertsRouteReturnsSnakeCaseDTO(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	nodeID := createDashboardNode(t, api, "cert-node")
	// 基准必须跟随真实时钟:handler 走 nowUTC(),fixture 若钉死日历日,
	// 过了那天断言就会漂移(本测试曾硬编码 2026-08-16 并在 2026-08-18 挂掉)。
	now := time.Now().UTC()
	revokedAt := now.Add(-48 * time.Hour)
	// 过期 15.5 天:daysUntilExpiry 把不足一天的零头向上取整为整天,15.5 天稳定
	// 落在 -16。刻意避开整天边界,测试执行耗时(乃至几小时)都不会让它跳到 -15。
	expiredAt := now.Add(-15*24*time.Hour - 12*time.Hour)
	certs := []NodeCert{
		{
			NodeID: nodeID, Fingerprint: "fp-expired", Serial: "1",
			NotBefore: now.Add(-90 * 24 * time.Hour), NotAfter: expiredAt,
			Revoked: false,
		},
		{
			NodeID: nodeID, Fingerprint: "fp-revoked", Serial: "2",
			NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour),
			Revoked: true, RevokedAt: &revokedAt,
		},
	}
	for i := range certs {
		if err := api.db.Create(&certs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	rw := serve(api, http.MethodGet, "/api/admin/nodes/1/certs", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("list certs status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("certs = %d, want 2", len(raw))
	}
	first := raw[0]
	for _, key := range []string{"id", "fingerprint", "serial", "not_before", "not_after", "revoked", "created_at", "status", "days_until_expiry"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("cert response missing snake_case key %q: %v", key, first)
		}
	}
	// 大驼峰旧字段名不得再出现(NodeID 已从 DTO 中移除)。
	for _, key := range []string{"ID", "NodeID", "Fingerprint", "Serial", "NotAfter", "Revoked"} {
		if _, ok := first[key]; ok {
			t.Fatalf("cert response must not carry Go-style key %q", key)
		}
	}
	if first["status"] != certStatusExpired {
		t.Fatalf("expired-but-not-revoked status = %v, want %q", first["status"], certStatusExpired)
	}
	days, ok := first["days_until_expiry"].(float64)
	if !ok || days != -16 {
		// 过期 15.5 天 -> 向上取整到已流逝的第 16 个整天,与运行日期无关。
		t.Fatalf("days_until_expiry = %v, want -16", first["days_until_expiry"])
	}
	second := raw[1]
	if second["status"] != certStatusRevoked {
		t.Fatalf("revoked status = %v, want %q", second["status"], certStatusRevoked)
	}
}
