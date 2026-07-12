# 实施计划

## Implementation

1. 新增共享项目配置和 `ProjectMeta` 组件，集中管理 GitHub URL 与编译期版本。
2. 重构 `App.vue` 的品牌、导航、退出操作和侧栏底部信息层级。
3. 在 `Login.vue` 中补充产品标识和共享项目信息。
4. 调整 `styles.css` 的应用外壳、内容宽度、焦点态、卡片细节和响应式布局。
5. 在 Vite 配置与类型声明中加入 `APP_VERSION` 注入及 `dev` 回退。
6. 更新 Release workflow 和 Dockerfile，使二进制与镜像前端均接收发布 Tag。

## Validation

- `cd pkg/webadmin/ui && npm run build`
- 检查默认构建产物包含 `dev` 与 GitHub HTTPS 地址。
- 使用 `APP_VERSION=0.7.2-test npm run build`，检查产物包含发布版本且不包含错误回退。
- 检查 Release workflow 的 UI build env 与 Docker build arg 使用同一 tag 输出。
- 使用真实 Chrome 检查登录页、仪表盘、GitHub 外链、版本文字和导航。
- 在桌面宽度与小于 900px 的移动宽度检查无页面级横向溢出。

## Risk and Rollback Points

- `App.vue` 与全局 CSS 会影响全部管理页；浏览器检查必须覆盖仪表盘和至少一个表格页。
- Docker `ARG` 具有构建阶段作用域；必须定义在 web stage 内并验证 Buildx 参数名一致。
- 若版本注入导致构建失败，先回滚 Vite define、类型声明和发布参数，UI 视觉改动可独立保留。

## Review Gate

- PRD、技术设计和实施计划经用户确认后，再运行 `task.py start` 进入实现阶段。
