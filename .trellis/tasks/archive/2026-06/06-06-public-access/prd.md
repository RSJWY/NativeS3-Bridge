# 对象存储公开访问能力（Bucket ACL + 匿名下载）— 父任务

> 本文件是本次扩展的**需求总纲**与**子任务地图**。父任务本身不直接产出实现代码，
> 只负责：源需求集合、子任务划分、跨子任务验收标准、最终集成评审。
> 本任务在既有 `06-05-natives3-bridge` 系统之上做**受控演进**，不得破坏既有私有桶的安全模型。

---

## ⛔ 给任务执行者的硬约束（最高优先级，禁止违反）

执行本任务树的开发者（人或 AI）必须严格遵守以下条款。这些条款由规划者制定，**执行者无权修改、删减、重新解释或"优化"任务细节**。

1. **任务细节锁定**：本任务树中所有 `prd.md` / `design.md` / `implement.md` 的需求、接口契约、目录结构、数据库表结构、技术选型、验收标准，均为**冻结规格**。执行者不得擅自更改。
2. **发现冲突先上报**：若执行中发现规格存在矛盾、无法实现、或明显缺陷，**停止该项实现并在任务的 research/ 目录写 change-request.md**，交回规划者裁决。禁止"我觉得这样更好"式的自行改动。
3. **不得缩减范围**：禁止以"先简化""后续再补"为由删除任一已列需求或验收项。
4. **不得替换技术选型**：沿用既有系统的 Go + 标准库 net/http + GORM 三驱动 + Vue3/Vite/ECharts + go:embed，禁止引入新框架或新重依赖（新增依赖须规划者批准）。
5. **复用既有约定**：S3 错误统一走 `pkg/handlers/common.go` 的 `WriteS3Error`；日志用 `log/slog`；时间 UTC；鉴权身份通过 `auth` 包的 context 注入。禁止自造平行机制。
6. **逐项勾验收**：每个子任务完成时，必须逐条对照该任务 `prd.md` 的 Acceptance Criteria 自检并勾选；未全绿不得标记完成。

---

## 背景与目标

当前系统（`06-05-natives3-bridge`）的 S3 端口对**每一个请求**强制 SigV4 校验（见 `pkg/server/router.go` 的 `Auth` 中间件 → `pkg/auth/authenticator.go` 的 `Verify`），没有任何匿名放行分支。父任务 `06-05` 的 PRD 把"未带合法签名的请求被拒绝"列为验收红线。

本次目标：让系统支持类云厂商（AWS S3 / 阿里云 OSS）的**"签名上传 + 公开下载"**访问模型：

- 管理员可将某个 **bucket 设为 `public-read`**；
- `public-read` 桶内的对象，**任何 HTTP 客户端无需任何凭证即可 GET/HEAD 下载**；
- **上传、删除、列举管理操作仍必须 SigV4 签名**；
- 私有桶（默认）维持现状，一切操作都要签名。

### 与既有红线的关系（规划者裁决记录）

本次新增"公开匿名只读"相当于对 `06-05` 红线"未签名一律拒绝"开一个**受控的、可管理的、默认关闭的例外**。裁决如下，执行者须遵守此边界：

- 例外**仅适用于** ACL 为 `public-read` 的 bucket 的 **GET / HEAD 对象**请求（含 Range）。
- 例外**绝不适用于**：PutObject、DeleteObject、Multipart 任意操作、Tagging 写、所有管理 API。这些**永远需要签名**。
- bucket 默认 ACL 为 `private`，必须管理员显式设为 `public-read` 才生效。
- 1:1 原生文件映射红线**完全不变**：公开访问只改"谁能读"，不改文件落地形态。

---

## 访问模型矩阵（冻结，作为最终验收基准）

| 请求 | private 桶 | public-read 桶 |
|---|---|---|
| GET / HEAD 对象（带合法签名） | ✅ 允许 | ✅ 允许 |
| GET / HEAD 对象（**无签名 / 匿名**） | ❌ 403 | ✅ **允许（本次新增）** |
| GET / HEAD 对象（预签名 URL，有效期内） | ✅ 允许 | ✅ 允许 |
| PUT 对象（无签名） | ❌ 403 | ❌ **403（写永远需签名）** |
| DELETE 对象（无签名） | ❌ 403 | ❌ **403** |
| ListObjectsV2（无签名，列目录） | ❌ 403 | ❌ **403（公开不可列举，防枚举）** |
| 任意管理 API | 需 session | 需 session |

> 公开桶**只读单个已知对象**，不允许匿名列举桶内容（防止信息枚举泄露）。如未来要放开匿名列举，须规划者另行裁决。

---

## 子任务地图（按依赖顺序执行）

| 顺序 | 子任务目录 | 交付物 | 依赖 |
|---|---|---|---|
| 1 | `06-06-bucket-model` | `Bucket` GORM 模型 + AutoMigrate；CreateBucket/DeleteBucket S3 接口；bucket ACL 存取与缓存；既有桶的兼容处理 | 无（基于 06-05 已完成系统） |
| 2 | `06-06-anon-read` | 鉴权中间件改造：识别匿名 GET/HEAD，查 bucket ACL，public-read 放行、其余 403；统计与 hooks 兼容匿名身份 | 1 |
| 3 | `06-06-bucket-admin-ui` | 管理 API（列桶/建桶/删桶/设 ACL）+ Vue 桶管理界面（列表、建删、公开开关） | 1（API 契约），可与 2 并行 |

> 父子结构本身不是依赖系统。执行者须按本表顺序：先做 1（数据模型与 ACL 存储是 2、3 的共同地基），2 与 3 在 1 完成后可并行。

---

## 跨子任务集成验收标准（父任务负责最终验收）

- [ ] 同一份二进制 `go build` 仍产出单文件，三驱动（sqlite/mysql/postgres）均能自动迁移出 `buckets` 表并正常启动。
- [ ] 管理员在后台或经 API 新建一个 bucket，磁盘上对应目录被创建，DB 中有该 bucket 记录，默认 ACL = `private`。
- [ ] 新建 bucket 默认 `private`：匿名 `curl http://host:9000/<bucket>/<key>` 返回标准 S3 403 XML。
- [ ] 管理员将该 bucket 设为 `public-read` 后，匿名 `curl`（无任何凭证、无签名）能直接 GET 下载该对象，内容字节一致；HEAD 同样可取回元数据。
- [ ] 对 public-read 桶的匿名 Range 请求返回 206 且字节正确。
- [ ] 对 public-read 桶的匿名 **PUT/DELETE 仍返回 403**；匿名 **ListObjectsV2 仍返回 403**。
- [ ] 带合法签名的 PUT 上传到 public-read 桶**成功**（签名上传 + 公开下载闭环成立）。
- [ ] 将 bucket 改回 `private` 后，原先可匿名访问的对象再次匿名请求返回 403（ACL 变更在缓存 TTL 内生效，TTL 须在文档写明）。
- [ ] 删除 bucket：DB 记录与磁盘目录的处理符合各子任务规格（非空桶删除行为须明确定义并验收）。
- [ ] 既有（06-05 时期）直接在磁盘上存在、但 DB 无记录的"历史桶"，访问行为有明确兼容策略（默认按 private 处理），不导致 panic 或 500。
- [ ] 管理后台新增"桶管理"页：可看到桶列表及其 ACL，可建桶、删桶、切换 public-read/private，操作后 S3 侧行为随之变化。
- [ ] 全程未触碰 1:1 原生映射红线；公开访问不改变文件落地形态。
- [ ] `go build ./...` / `go vet ./...` / `go test ./...` 全绿。

---

## Notes

- 各子任务 `prd.md` 给出细化需求与独立验收；`design.md` 给出接口契约与数据流；`implement.md` 给出有序执行清单与验证命令。
- 本任务树语言：中文（与用户沟通一致）。代码注释与标识符用英文。
- 关键既有代码锚点（供执行者定位，非冻结，以实际代码为准）：
  - 鉴权中间件：`pkg/server/router.go` `Auth` / `Quota`
  - 签名校验：`pkg/auth/authenticator.go` `Verify` / `HasPresignQuery`
  - 凭证缓存范式（ACL 缓存可仿照）：`pkg/auth/credential_store.go`
  - 数据模型：`pkg/db/models.go`；迁移：`pkg/db/migrate.go`
  - 桶路径与命名校验：`pkg/storage/path.go` `ValidateBucketName` / `ResolveBucketPath`
  - 对象读：`pkg/handlers/object.go` `Get` / `Head` / `ListObjectsV2`
  - 管理 API 风格：`pkg/webadmin/api.go`、路由注册 `pkg/webadmin/server.go`
  - 前端视图：`pkg/webadmin/ui/src/views/`（Credentials.vue 可作桶管理页范本）
