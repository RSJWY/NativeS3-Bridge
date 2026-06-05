# 子任务 3 设计：SigV4 鉴权与按密钥配额

> 仅细化本子任务。全局以父任务 `design.md` 为准。

## 1. SigV4 校验流程（pkg/auth/sigv4.go）

标准 AWS4-HMAC-SHA256：
```
1. 解析 Authorization 头:
   AWS4-HMAC-SHA256 Credential=<AK>/<date>/<region>/s3/aws4_request,
   SignedHeaders=host;x-amz-date;..., Signature=<hex>
2. 取 X-Amz-Date，校验与服务器时间偏移 ≤ 15min，否则 RequestTimeTooSkewed
3. 用 AK 查 credential_store 取 SecretKey；查不到→InvalidAccessKeyId；disabled→AccessDenied
4. 重建 CanonicalRequest（method, canonical uri, canonical query, canonical headers,
   signed headers, hashed payload）。payload hash 取 X-Amz-Content-Sha256
   （支持 UNSIGNED-PAYLOAD）。
5. 构造 StringToSign，派生 signing key（date→region→s3→aws4_request 链式 HMAC-SHA256）
6. 计算签名与请求中 Signature 比对（hmac.Equal 常量时间比较）；不等→SignatureDoesNotMatch
7. 通过→返回 *Identity{CredentialID, AccessKey, QuotaBytes, UsedBytes}
```

复用约束：CanonicalRequest / signing key 派生函数要做成可被 S5 预签名（query 形式）复用的纯函数。

## 2. 凭证缓存（pkg/auth/credential_store.go）

```go
type CredentialStore struct {
    db    *gorm.DB
    cache map[string]cachedCred   // AccessKey → {cred, expireAt}
    ttl   time.Duration           // 默认 60s
    mu    sync.RWMutex
}
func (s *CredentialStore) Get(accessKey string) (*db.Credential, error)
func (s *CredentialStore) Invalidate(accessKey string)   // 供管理端改密钥后调用
```
- 缓存未命中或过期→查 DB；`status=disabled` 也缓存（避免被禁密钥反复打 DB）。
- 管理端创建/禁用/删除密钥后应调用 `Invalidate`（接口暴露给 webadmin 子任务）。

## 3. 配额（pkg/quota/quota.go）

```go
func Check(id *auth.Identity, incoming int64) error {
    if id.QuotaBytes > 0 && id.UsedBytes+incoming > id.QuotaBytes {
        return ErrQuotaExceeded   // → handler 翻译为 S3 403
    }
    return nil
}

// 单事务：原子自增 used_bytes + upsert request_stats
func Commit(db *gorm.DB, credID uint, deltaBytes int64, op Op) error
//   op ∈ {OpPut, OpGet, OpDelete}
//   OpPut:    used_bytes += delta(>0); put_count++; bytes_in += delta
//   OpDelete: used_bytes  = max(0, used_bytes - delta); delete_count++
//   OpGet:    get_count++; bytes_out += delta
```
- `used_bytes` 更新：`UPDATE credentials SET used_bytes = MAX(0, used_bytes + ?) WHERE id = ?`（注意三驱动 MAX/GREATEST 兼容：用 `CASE WHEN` 或先读后写在事务+行锁内，二选一，记入 research）。
- `request_stats` upsert：`ON CONFLICT(credential_id, day)`（sqlite/pg）/ `ON DUPLICATE KEY`（mysql）——用 GORM `clause.OnConflict` 屏蔽方言差异。

## 4. 中间件接入（pkg/server/router.go，替换 S2 占位）

```
Auth 中间件:
  id, err := authenticator.Verify(r)
  if err != nil { writeS3Error(...); return }
  ctx = context.WithValue(ctx, ctxKeyIdentity, id)

Quota 中间件（仅写方法）:
  if isWrite(r) {
     size := r.ContentLength
     if err := quota.Check(id, size); err != nil { writeS3Error 403; return }
  }

Handler 成功后:
  PUT    → quota.Commit(db, id.CredentialID, size, OpPut)
  DELETE → quota.Commit(db, id.CredentialID, deletedSize, OpDelete)
  GET    → quota.Commit(db, id.CredentialID, sentBytes, OpGet)
```
- Identity 从 context 取，handler 不直接碰 auth 细节。
- Commit 失败只记日志，不影响已成功的对象操作（统计最终一致性优先于强一致）；但 used_bytes 增减失败需告警日志。

## 5. 错误码映射（统一走 handlers/common.go）
| 场景 | Code | HTTP |
|---|---|---|
| 签名不匹配 | SignatureDoesNotMatch | 403 |
| AccessKey 不存在 | InvalidAccessKeyId | 403 |
| 密钥禁用 | AccessDenied | 403 |
| 时钟偏移 | RequestTimeTooSkewed | 403 |
| 缺 Authorization | AccessDenied | 403 |
| 配额超限 | QuotaExceeded（自定义，沿用 S3 风格 XML） | 403 |

## 6. 并发正确性
- used_bytes 自增用单条原子 UPDATE，杜绝读改写竞态。
- Check 与 Commit 之间存在 TOCTOU 窗口（先检查后提交）：第一版接受少量超额（软配额），在 research 标注；如需硬配额，用事务内"加锁读 used_bytes→判断→更新"。默认软配额。
