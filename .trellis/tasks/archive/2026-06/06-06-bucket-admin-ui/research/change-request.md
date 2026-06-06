# Change Request: webadmin UI dist 提交策略冲突

## 背景

子任务 `06-06-bucket-admin-ui/prd.md` 的 C.7 与 Notes 要求：

- 前端改动后运行 `npm run build`；
- 更新并提交 `pkg/webadmin/ui/dist/`，以保证 `go:embed all:dist` 的单二进制包含新页面。

但项目当前规范与仓库配置要求相反：

- `.trellis/spec/backend/webadmin-guidelines.md` Common Mistakes 明确写明：`Do not commit built dist/assets/*; keep only .gitkeep tracked. Build artifacts are regenerated before embedding and are ignored by .gitignore.`
- `.gitignore` 忽略 `pkg/webadmin/ui/dist/*`，仅反忽略 `pkg/webadmin/ui/dist/.gitkeep`。
- 当前 `git ls-files 'pkg/webadmin/ui/dist/**'` 仅跟踪 `.gitkeep`。

## 冲突

若遵循本任务 PRD 提交 dist 产物，需要修改 `.gitignore` 或强制添加被忽略文件，这违反现有项目 spec；若遵循 spec 不提交 dist，则违反本任务 PRD 的 dist 提交要求。

## 请求裁决

请规划者裁决以下二选一：

1. **保持现有项目策略**：不提交 `pkg/webadmin/ui/dist/**` 构建产物；在发布/构建流程中要求先运行 `npm run build`，再运行 `go build` 以完成嵌入。同步更新本任务 PRD/Acceptance Criteria，移除“dist 需提交”的要求。
2. **改变仓库策略**：允许提交 webadmin dist 产物；修改 `.gitignore` 以跟踪 `pkg/webadmin/ui/dist/index.html` 和 `pkg/webadmin/ui/dist/assets/*`，并更新 `.trellis/spec/backend/webadmin-guidelines.md` 的 Common Mistakes。

## 检查代理建议

建议选择方案 1，保持现有 spec 与 `.gitignore` 策略不变。当前本地 `npm run build` 已成功生成 dist，`go build ./...` 可在本地嵌入这些被忽略产物；但它们不会作为代码评审/提交内容出现。
