# 技术设计

## Scope and Boundaries

本次改动限定在管理端应用外壳、共享项目信息组件、前端构建配置和发布构建参数。业务页面数据、后端 API、认证状态与 ECharts 数据流保持不变。

## UI Structure

- 新增共享 `ProjectMeta` 组件，统一渲染 GitHub 链接和版本号，避免 `App.vue` 与 `Login.vue` 重复常量和标记。
- `App.vue` 继续保留固定宽度的标准侧栏，调整为品牌区、主导航和底部操作/项目信息三个稳定层级。
- `Login.vue` 保留单卡片登录结构，在卡片外补充产品名与共享项目信息，不创建营销式 Hero。
- `styles.css` 沿用现有暖灰变量，统一内容最大宽度、边界、焦点态、侧栏高度和移动端重排；不引入渐变、玻璃效果或夸张阴影。

## Version Data Flow

```text
Release tag -> APP_VERSION build variable -> Vite define __APP_VERSION__
            -> shared project config -> ProjectMeta -> rendered version

Local build without APP_VERSION -> "dev" fallback -> same rendering path
```

- `vite.config.ts` 读取并清理 `APP_VERSION`，空值回退为 `dev`，通过 Vite `define` 生成编译期常量。
- `env.d.ts` 声明该只读全局常量，保持 Vue TypeScript 严格检查通过。
- `.github/workflows/release.yml` 在普通前端构建和 Docker Buildx 构建中传入同一个 Release Tag。
- `Dockerfile` 声明 `APP_VERSION` build arg，并仅在 web 构建阶段传给 Vite。
- GitHub 地址作为共享前端常量固定为仓库的 HTTPS URL。

## Compatibility and Security

- 未提供构建参数时始终显示 `dev`，因此本地和旧式构建命令继续工作。
- GitHub 外链使用 `target="_blank"` 与 `rel="noopener noreferrer"`。
- 不新增运行时网络请求，版本显示不依赖 GitHub 可用性，也不暴露额外服务信息。
- 移动端沿用 900px 断点，侧栏转为顶部区域，导航与项目信息自然换行。

## Rollback

UI 改动、共享组件和构建参数均无持久化状态。回滚相关前端与构建文件即可恢复，不需要数据迁移。
