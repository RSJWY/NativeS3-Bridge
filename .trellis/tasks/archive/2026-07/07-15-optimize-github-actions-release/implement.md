# 优化 GitHub Actions 双镜像发布 — 执行计划

## 1. Workflow 改造

- [x] 将 release metadata 提取为 `prepare` job outputs。
- [x] 新增最小权限 `quality` job，构建 UI并执行 Go 1.21 vet/test/race、发布契约检查。
- [x] 新增 `artifacts` job，构建 panel/node 的 10 个跨平台归档和 checksums。
- [x] 新增 `images` matrix job，分别发布 panel/node 多架构镜像。
- [x] 显式设置 Docker target、cache scope、provenance、SBOM、OCI labels。
- [x] 新增 `release` job 下载制品并创建 Release。

## 2. 文档同步

- [x] 将 README Docker 部署章节改为 panel/node 双容器入口。
- [x] 更新 GHCR 镜像地址与本地 build target。
- [x] 更新发布流程、二进制归档名称和 attestation/untagged digest 说明。
- [x] 明确 legacy `natives3-bridge` 不再更新。

## 3. 验证

- [x] `actionlint .github/workflows/release.yml`。
- [x] YAML 解析和 shell 片段语法检查。
- [x] `npm ci && npm run build`。
- [x] Go 1.21 `go vet ./...`、`go test ./...`、`go test -race ./...`。
- [x] 本地等价交叉编译 panel/node 的全部目标并检查归档内容。
- [x] `bash scripts/test-distribution-contract.sh`。

## 风险文件与回滚点

- `.github/workflows/release.yml`：发布核心；任何 expression/job dependency 错误会阻断发布，但不会影响运行时代码。
- `README.md`：镜像和制品契约必须与 workflow 一致。
- 不修改 Dockerfile 运行时代码；workflow 只消费已验证的 `panel`/`node` target。
