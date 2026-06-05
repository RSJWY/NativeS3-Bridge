# 子任务 5 设计：预签名 URL 与事件钩子

> 仅细化本子任务。全局以父任务 `design.md` 为准。

## 1. 预签名校验（pkg/handlers/presigned.go）

query 形式 SigV4 参数：
```
X-Amz-Algorithm=AWS4-HMAC-SHA256
X-Amz-Credential=<AK>/<date>/<region>/s3/aws4_request
X-Amz-Date=<ISO8601>
X-Amz-Expires=<seconds>
X-Amz-SignedHeaders=host
X-Amz-Signature=<hex>
```

校验流程（复用 S3 的纯函数）：
```
1. 从 query 取上述参数；缺失→走 header 鉴权或 403
2. AK→credential_store 取 secret（同 S3）
3. 过期检查: now > parse(X-Amz-Date) + X-Amz-Expires → 403 AccessDenied
4. CanonicalRequest 的 canonical query 要剔除 X-Amz-Signature 本身，其余 query 参与签名
5. payload hash 用 UNSIGNED-PAYLOAD（预签名惯例）
6. 重算签名比对（hmac.Equal）；不等→403
7. 通过→注入 Identity，转交 GET/PUT object handler
```

Auth 中间件分支：
```go
if hasPresignQuery(r) {
    id, err = presign.Verify(r)
} else {
    id, err = headerAuth.Verify(r)
}
```

可选生成函数：
```go
func GeneratePresignedURL(cred db.Credential, method, bucket, key string, expires time.Duration) (string, error)
```

## 2. 事件钩子（pkg/hooks）

```go
// event.go
type EventType string
const ( ObjectCreated EventType = "ObjectCreated"; ObjectDeleted EventType = "ObjectDeleted" )

type Event struct {
    Type         EventType         `json:"type"`
    Bucket       string            `json:"bucket"`
    Key          string            `json:"key"`
    Size         int64             `json:"size"`
    ETag         string            `json:"etag"`
    Metadata     map[string]string `json:"metadata,omitempty"`
    CredentialID uint              `json:"credential_id"`
    Timestamp    string            `json:"timestamp"`   // RFC3339；由 handler 注入（避免脚本内 Date 限制）
}
```

```go
// manager.go
type Manager struct {
    db       *gorm.DB
    hooks    []Hook       // 从 hook_configs 加载
    queue    chan Event   // 缓冲 channel，默认 cap=1024
    workers  int          // 默认 4
    maxRetry int          // 默认 3
}
func NewManager(db, cfg) *Manager
func (m *Manager) Start()              // 启动 worker
func (m *Manager) Emit(e Event)        // 非阻塞: select{ case queue<-e: default: 丢弃+告警日志 }
func (m *Manager) Reload() error       // 重新从 DB 读 hook_configs
func (m *Manager) Stop()
```

- worker 从 queue 取事件 → 对每个匹配 type 的 hook 调用 `Deliver` → 失败按指数退避重试（如 1s/2s/4s），超 maxRetry 记错误日志。
- queue 满时 Emit 丢弃并告警（保护主路径不阻塞）。

```go
// webhook.go
type WebhookHook struct { URL string; Events []EventType; client *http.Client /*timeout 5s*/ }
func (h *WebhookHook) Match(t EventType) bool
func (h *WebhookHook) Deliver(e Event) error   // POST JSON, 非 2xx 视为失败
```

## 3. 触发点接入
| 操作 | 触发 | 载荷来源 |
|---|---|---|
| PutObject 成功 | Emit(ObjectCreated) | ObjectInfo + Identity |
| CompleteMultipartUpload 成功 | Emit(ObjectCreated) | 合并后 ObjectInfo |
| DeleteObject 成功 | Emit(ObjectDeleted) | bucket/key/被删 size |

- Timestamp 在 handler 处用服务器时间生成后传入（脚本/工作流环境不可用 Date，但这是 Go 运行时，正常使用 `time.Now().UTC()`）。

## 4. 装配（main.go）
- 创建 `hooks.Manager` 并 `Start()`；注入到 handler 依赖。
- 预签名 Verify 注入 Auth 中间件分支。
- webadmin 子任务改钩子配置后调用 `Manager.Reload()`。

## 5. 配置/常量（记入 research，必要时加 config）
- `hooks.queue_size`（默认 1024）、`hooks.workers`（4）、`hooks.max_retry`（3）、`hooks.timeout`（5s）。
