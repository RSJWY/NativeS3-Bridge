# 子任务 2：匿名公开下载鉴权改造（公开桶放行 GET/HEAD）

> 父任务：`06-06-public-access`。本子任务实现核心行为：对 `public-read` 桶的匿名 GET/HEAD 放行，
> 其余一切仍强制签名。执行者硬约束见父任务 `prd.md`；冲突写 `research/change-request.md` 上报。

## Goal

改造 S3 鉴权中间件：当请求是**无签名/匿名**的 **GET 或 HEAD 对象**请求，且目标 bucket 的 ACL 为 `public-read` 时，**放行**为匿名身份继续处理；其余所有情形维持既有签名校验（失败 403）。保证私有桶安全模型零回退。

## 依赖
- 子任务 1（`06-06-bucket-model`）提供 `BucketStore.GetACL`。本任务**必须**在子任务 1 合并后实现。

## Requirements

### A. 匿名识别与放行
1. 在鉴权层（`pkg/server/router.go` 的 `Auth` 中间件，或抽到 `pkg/auth`）区分：
   - **带凭证**：请求含 `Authorization` 头（header SigV4）**或** 含预签名 query 参数（`auth.HasPresignQuery`）→ 走既有 `authenticator.Verify`，行为完全不变。
   - **无凭证（匿名）**：既无 `Authorization` 头、也无预签名参数。
2. 对**匿名**请求，仅当**全部**满足时放行：
   - method 为 `GET` 或 `HEAD`；
   - 解析出的 `bucket != ""` 且 `key != ""`（即针对**单个对象**，不是桶级/服务级）；
   - 不带任何"写/管理"语义子资源：**禁止**匿名访问 `?tagging`、`?uploads`、`?uploadId`、`?acl` 等（带这些一律按需签名或 403）；
   - `BucketStore.GetACL(bucket)` 返回 `exists=true && acl=="public-read"`。
3. 不满足放行条件的匿名请求 → 返回标准 S3 403（`AccessDenied`）XML，**不得泄露**桶是否存在等信息（统一 403，不区分 NoSuchBucket 与私有）。
4. 放行后，注入一个**匿名身份**到 context（见 B），交由既有 object handler 处理 GET/HEAD（含 Range/206）。

### B. 匿名身份与下游兼容
5. 定义匿名身份表示（如 `auth.AnonymousIdentity` 或 `Identity{CredentialID:0, AccessKey:"anonymous"}`，具体由 design 定）。要求：
   - 既有依赖 `IdentityFromContext` 的代码在匿名时不 panic、不 500。
   - **Quota 中间件**：匿名只允许 GET/HEAD（非 PUT），其配额检查分支（仅作用于 PUT）天然不触发；须验证匿名 GET 不会因取不到 Identity 而报错。
   - **用量统计 / hooks**：匿名 GET 的统计归属须明确——是否累加到某虚拟身份或跳过。design 给出策略（建议：匿名读**跳过**按密钥统计，避免脏数据；或单列 anonymous 统计，二选一并写明）。匿名读不触发 ObjectCreated/Deleted（本就只读）。
6. **写操作绝对不放行**：匿名 PUT/DELETE/POST(multipart)/PutTagging 一律 403，无论桶 ACL。须有测试覆盖 public-read 桶下的匿名 PUT/DELETE = 403。

### C. 安全边界
7. 匿名 `ListObjectsV2`（`GET /{bucket}` 无 key）→ **403**（即使 public-read），防止枚举桶内容。
8. 路径穿越防护沿用既有 `ResolveObjectPath`，匿名路径不得绕过。
9. ACL 变更生效：依赖子任务 1 的缓存 TTL（默认 60s，单进程即时失效）。改回 private 后匿名请求在 TTL 内转为 403——验收用单进程即时生效路径（SetACL 已 Invalidate）。

## 非目标
- 不实现 bucket ACL 的设置入口（子任务 1 提供 store，子任务 3 提供 UI/API）。
- 不实现匿名写、匿名列举。
- 不实现对象级 ACL。

## Acceptance Criteria
- [ ] `go build ./...` / `go vet ./...` / `go test ./...` 全绿。
- [ ] private 桶匿名 `curl -s -o /dev/null -w '%{http_code}' http://host:9000/b/k` → 403，响应体为标准 `<Error>` XML。
- [ ] public-read 桶匿名 GET 对象 → 200，下载内容与磁盘字节一致。
- [ ] public-read 桶匿名 HEAD 对象 → 200，返回 Content-Length/ETag/`x-amz-meta-*`。
- [ ] public-read 桶匿名 Range 请求 → 206，区间字节正确。
- [ ] public-read 桶匿名 `PUT` → 403；匿名 `DELETE` → 403；匿名 `GET /{bucket}`(列举) → 403。
- [ ] 带合法签名的所有操作（GET/PUT/HEAD/DELETE/List/multipart/预签名）在 private 与 public-read 桶下行为与改造前一致（回归通过）。
- [ ] 匿名 GET 不污染按密钥用量统计（或按 design 既定策略，单测验证）。
- [ ] 单测覆盖：匿名放行判定矩阵（method × 是否带凭证 × ACL × 是否对象级）。

## Notes
- 中间件链顺序敏感：`Auth` 决定放行/拒绝，`Quota` 仅管 PUT。改造须保证匿名 GET 走过 `Auth`（放行注入匿名身份）后，`Quota` 不误拦。design 须画出新的判定流程图。
- 统一 403 不区分"桶不存在/私有/无权限"，避免信息泄露，与父任务安全裁决一致。
