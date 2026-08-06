package panel

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// PublishedCredentialView 是已发布快照里凭证的脱敏视图。它**刻意不含**
// SecretKey 与 SecretKeyCipher 字段——这是脱敏红线的类型级保证:
// 字段不存在,就无法被 json.Marshal 误序列化进 HTTP 响应。
// 直接序列化 controlproto.DesiredCredential 是错误的——它的 SecretKey
// 字段没有 omitempty,会把明文 secret 写进响应。
type PublishedCredentialView struct {
	AccessKey  string `json:"access_key"`
	Name       string `json:"name,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Status     string `json:"status"`
	QuotaBytes int64  `json:"quota_bytes"`
}

// PublishedBucketView / PublishedWebhookView 是已发布快照里桶与 webhook 的视图。
// 它们与 controlproto.DesiredBucket / DesiredWebhook 字段一致,但定义独立类型,
// 便于将来脱敏/裁剪而不影响 wire 协议。Webhook 的 Events 在后端就拆成数组,
// 与草稿 API 的表现形态一致,减少前端特殊处理。
type PublishedBucketView struct {
	Name string `json:"name"`
	ACL  string `json:"acl"`
}

type PublishedWebhookView struct {
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
}

type PublishedRateLimitView struct {
	AnonymousRPS   float64 `json:"anonymous_rps"`
	AnonymousBurst int     `json:"anonymous_burst"`
	TrustForwarded bool    `json:"trust_forwarded"`
}

// PublishedSnapshotView 是 GET /api/admin/nodes/{id}/desired-state 的响应。
// 它直接从 persistedDesiredSnapshot 构造,**不调用 decryptSnapshot**——
// 明文 secret 全程不进内存,脱敏是结构性保证而非"记得删字段"。
// 读路径不做 hash 完整性校验(push 路径的校验保持原样),但带上存储的
// content_hash 供管理员与节点上报的 hash 人工对照。
type PublishedSnapshotView struct {
	Published       bool                      `json:"published"`
	Version         int64                     `json:"version"`
	ContentHash     string                    `json:"content_hash"`
	SchemaVersion   int                       `json:"schema_version"`
	RepublishNeeded bool                      `json:"republish_needed"`
	UpdatedBy       string                    `json:"updated_by,omitempty"`
	UpdatedAt       *string                   `json:"updated_at,omitempty"`
	Credentials     []PublishedCredentialView `json:"credentials"`
	Buckets         []PublishedBucketView     `json:"buckets"`
	Webhooks        []PublishedWebhookView    `json:"webhooks"`
	RateLimit       *PublishedRateLimitView   `json:"rate_limit,omitempty"`
}

// PublishedView 返回已发布快照的脱敏视图,不解密任何 secret。
// 设计见 .trellis/tasks/08-06-panel-node-config-view/design.md §2.2/§3。
//
// 状态机:
//   - 无 desired_configs 行 -> Published=false, 空切片(非 nil), 无 error
//   - 旧格式快照(ErrDesiredSnapshotRepublishRequired)或其它 decode 错误 ->
//     Published=true, RepublishNeeded=true, 空内容, 无 error(fail closed)
//   - 正常 -> Published=true, 完整脱敏内容
func (a *DesiredStateAuthority) PublishedView(nodeID uint) (PublishedSnapshotView, error) {
	view := PublishedSnapshotView{
		Published:   false,
		Credentials: []PublishedCredentialView{},
		Buckets:     []PublishedBucketView{},
		Webhooks:    []PublishedWebhookView{},
	}

	var config DesiredConfig
	err := a.db.Where("node_id = ?", nodeID).First(&config).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 节点存在但尚未发布是正常业务态,返回 200 空态而非 404/500。
			return view, nil
		}
		return PublishedSnapshotView{}, err
	}

	snapshot, err := decodePersistedDesiredSnapshot(config.ContentJSON)
	if err != nil {
		// 旧格式或 decode 错误:标记需重发布,内容为空,不回填草稿,不返回 error。
		view.Published = true
		view.Version = config.Version
		view.ContentHash = config.ContentHash
		view.RepublishNeeded = true
		view.UpdatedBy = config.UpdatedBy
		if t := formatSnapshotTime(config.UpdatedAt); t != "" {
			view.UpdatedAt = &t
		}
		return view, nil
	}

	view.Published = true
	view.Version = config.Version
	view.ContentHash = config.ContentHash
	view.SchemaVersion = snapshot.SchemaVersion
	view.UpdatedBy = config.UpdatedBy
	if t := formatSnapshotTime(config.UpdatedAt); t != "" {
		view.UpdatedAt = &t
	}

	for _, c := range snapshot.Credentials {
		view.Credentials = append(view.Credentials, PublishedCredentialView{
			AccessKey:  c.AccessKey,
			Name:       c.Name,
			Bucket:     c.Bucket,
			Status:     c.Status,
			QuotaBytes: c.QuotaBytes,
		})
	}
	for _, b := range snapshot.Buckets {
		view.Buckets = append(view.Buckets, PublishedBucketView{Name: b.Name, ACL: b.ACL})
	}
	for _, wh := range snapshot.Webhooks {
		view.Webhooks = append(view.Webhooks, PublishedWebhookView{
			URL:     wh.URL,
			Events:  splitWebhookEvents(wh.Events),
			Enabled: wh.Enabled,
		})
	}
	if snapshot.RateLimit != nil {
		view.RateLimit = &PublishedRateLimitView{
			AnonymousRPS:   snapshot.RateLimit.AnonymousRPS,
			AnonymousBurst: snapshot.RateLimit.AnonymousBurst,
			TrustForwarded: snapshot.RateLimit.TrustForwarded,
		}
	}
	return view, nil
}

// splitWebhookEvents 把 controlproto.DesiredWebhook.Events 的逗号分隔字符串
// 拆成 []string,与草稿 API 的表现形态一致。空字符串返回空切片(非 nil)。
func splitWebhookEvents(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// formatSnapshotTime 把 time.Time 格式化为 RFC3339 字符串;零值返回空。
// 用于响应里 updated_at:零值返回空串而非 "0001-01-01..."。
func formatSnapshotTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
