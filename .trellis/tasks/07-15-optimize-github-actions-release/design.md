# 优化 GitHub Actions 双镜像发布 — 技术设计

## 1. Job 拓扑

```text
prepare
  └─ quality
       ├─ artifacts ─┐
       └─ images ────┼─ release
                     ┘
```

- `prepare`：解析 tag/source_ref，checkout 并输出固定 commit SHA、Docker tag、GHCR namespace。
- `quality`：在固定 SHA 上构建 UI，执行 Go 1.21 vet/test/race 与发布静态契约检查。
- `artifacts`：重新构建 UI，交叉编译 panel/node，打包 10 个归档，生成 checksums，上传 workflow artifact。
- `images`：matrix=`panel,node`，分别 build/push Docker target；两个组件并行。
- `release`：等待 artifacts/images 全部成功，下载制品并创建 GitHub Release/tag。

该拓扑保证任何质量失败都不会发布，同时让 CPU 较重的跨平台归档和两个镜像构建并行执行。

## 2. 命名契约

### GitHub Release 归档

```text
natives3-panel-<version>-<os>-<arch>.tar.gz
natives3-node-<version>-<os>-<arch>.tar.gz
```

每个归档内部目录同名，二进制分别为 `panel[.exe]` / `node[.exe]`。

### GHCR

```text
ghcr.io/<lowercase-owner>/natives3-panel:<tag>
ghcr.io/<lowercase-owner>/natives3-panel:latest
ghcr.io/<lowercase-owner>/natives3-node:<tag>
ghcr.io/<lowercase-owner>/natives3-node:latest
```

旧 `natives3-bridge` 包保留为历史遗留，不再由新 workflow 更新。

## 3. Attestation 与 manifest

保留 BuildKit `provenance: mode=min`，并显式设置 `sbom: false`。每个双架构镜像的顶层 tag 指向 OCI index，index 包含：

- linux/amd64 runnable manifest
- linux/arm64 runnable manifest
- amd64 provenance attestation manifest (`unknown/unknown`)
- arm64 provenance attestation manifest (`unknown/unknown`)

GHCR 将子 manifest 和 attestation 显示为 untagged digest，这是预期结构。显式配置用于防止 Action 默认值将来变化。

## 4. 权限与可信边界

- workflow 默认 `contents: read`。
- `images` 额外 `packages: write`。
- `release` 额外 `contents: write`。
- 不引入第三方脚本下载执行；Action 使用 GitHub 官方、Docker 官方或现有 release Action。
- 所有下游 job 以 `prepare` 输出的 commit SHA checkout，避免 source_ref 在运行中漂移。

## 5. 缓存与可重复性

- Node 使用 npm lockfile cache。
- Go 使用 setup-go module/build cache。
- Buildx 使用 `type=gha`，cache scope 按 `panel` / `node` 分开，避免互相覆盖。
- GitHub Release 只消费 `artifacts` job 上传的固定制品，不依赖工作目录共享。
- checksums 在所有归档生成后一次性计算。

## 6. 回滚

- workflow 修改是单文件发布编排变更，可整体回退上一版本。
- 新包名不覆盖 legacy 包；若新流水线失败，旧 `natives3-bridge` 历史镜像仍存在，但不会被误标为新架构版本。
- Release 仅在两个镜像和所有归档成功后创建，避免半发布。
