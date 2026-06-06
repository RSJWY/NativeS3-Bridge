# 子任务 2 设计：匿名公开下载鉴权改造

> 细化本子任务内部设计，不得与父 `prd.md` 访问模型矩阵冲突。

## 1. 改动落点

```
pkg/auth/identity.go      # +匿名身份构造与判定（AnonymousIdentity）
pkg/server/router.go      # Auth 中间件改造：匿名分支 + ACL 查询；NewRouter 增 BucketStore/ACLLookup 参
pkg/auth/authenticator.go # （只读复用）HasPresignQuery 用于判定是否带凭证
cmd/natives3bridge/main.go# 注入 BucketStore.GetACL 给 router
pkg/server/router_test.go # 新增：放行判定矩阵测试（新文件）
```

> 不新增顶层包。ACL 查询通过函数类型注入，解耦 server 对 storage 的直接依赖：
> `type ACLLookup func(bucket string) (acl string, exists bool, err error)`，main.go 传 `bucketStore.GetACL`。

## 2. 是否"带凭证"的判定

```go
func hasCredentials(r *http.Request) bool {
    return r.Header.Get("Authorization") != "" || auth.HasPresignQuery(r)
}
```
- 带凭证 → 既有 `authenticator.Verify`，**逻辑完全不动**（含 header SigV4 与预签名）。
- 无凭证 → 进入匿名判定。

## 3. 匿名放行判定流程（冻结）

```
请求进入 Auth 中间件
 ├─ hasCredentials? ── 是 ──► authenticator.Verify(r)
 │                              ├─ ok  → 注入真实 Identity，next
 │                              └─ err → WriteS3Error 403（既有）
 └─ 否（匿名）
      ├─ method ∈ {GET, HEAD} ?            否 → 403 AccessDenied
      ├─ bucket!="" && key!="" ?           否（服务级/桶级/列举）→ 403
      ├─ 含写/管理子资源? (tagging/uploads/uploadId/acl) 是 → 403
      ├─ aclLookup(bucket) → (acl, exists, err)
      │     err  → 500 InternalError（slog.Error；不泄露细节）
      │     !exists → 403（历史桶/不存在，按 private）
      │     acl != "public-read" → 403
      │     acl == "public-read" → 注入 AnonymousIdentity，next
```

判定"含写/管理子资源"：检查 `r.URL.Query()` 是否含键 `tagging`/`uploads`/`uploadId`/`acl`/`tags`，命中即拒绝（匿名只走纯对象 GET/HEAD）。

## 4. 匿名身份

```go
// pkg/auth/identity.go
const AnonymousAccessKey = "anonymous"

func AnonymousIdentity() *Identity {
    return &Identity{CredentialID: 0, AccessKey: AnonymousAccessKey, QuotaBytes: 0, UsedBytes: 0}
}
func IsAnonymous(id *Identity) bool { return id != nil && id.CredentialID == 0 }
```

- `IdentityFromContext` 对匿名返回该身份（非 nil），下游不 panic。
- `Quota` 中间件：仅对 `PUT` 生效，匿名只会是 GET/HEAD，不进入配额分支；无需特殊改动，但**须加测试**确认匿名 GET 经过 Quota 不报错。

## 5. 统计与 hooks 策略（冻结决定）

- **匿名 GET 跳过按密钥用量统计**：`ObjectHandler.commitUsage` 对 `CredentialID==0` 的身份**不累加** RequestStat（避免把匿名流量记到不存在的 credential_id=0 上造成外键/脏数据）。在 commit 路径加 `if IsAnonymous(id) { return }` 短路（具体在 handler 的 commitUsage 或 quota.Commit 入口判断，design 选 handler 层短路，避免触达 DB）。
- 匿名读**不触发** hooks（读操作本就不触发 ObjectCreated/Deleted，保持不变）。
- 若未来要统计匿名流量，可单列 anonymous 维度——本期不做，记备查。

## 6. 错误与信息泄露

- 匿名被拒一律 `WriteS3Error(w, "AccessDenied", 403, path)`，**不区分** NoSuchBucket / private / 路径非法（路径非法仍可由下游 handler 自然返回，但 ACL 判定阶段统一 403）。
- ACL 查询 DB 故障 → `InternalError` 500 + `slog.Error`，不把 DB 错误透出客户端。

## 7. 中间件顺序

既有链：`Recover → Logging → Auth → Quota`。本改造只改 `Auth` 内部逻辑，**顺序不变**。匿名放行在 `Auth` 内完成身份注入，`Quota`/handler 下游无感知差异（除 commitUsage 短路）。

## 8. 兼容与回滚
- 带凭证路径零改动 → 既有签名功能全回归。
- 回滚：移除 `Auth` 的匿名分支、`AnonymousIdentity`、commitUsage 短路即可，恢复"一切需签名"。
- 私有桶（默认/历史桶）在改造后行为与改造前**完全一致**（匿名→403）。
