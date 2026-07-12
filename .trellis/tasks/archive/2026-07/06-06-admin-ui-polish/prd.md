# 管理后台 UI 美化与体验优化

## Goal

统一管理后台视觉风格（配色/排版/组件一致性），优化桶管理/凭证/Dashboard 页面交互、空状态、加载态与错误提示，提升整体观感与可用性。

## Requirements

- Keep the existing Vue3 + Vite + ECharts stack and admin API contracts; this task is frontend-only under `pkg/webadmin/ui`.
- Unify the admin UI visual language across login, shell navigation, dashboard, credentials, and buckets using the existing restrained warm-neutral palette, simple borders, normal cards/tables/forms, and no decorative gradients/glass/hero sections.
- Improve desktop and mobile layout so the sidebar/top navigation, page headers, action areas, charts, and tables remain readable at common narrow viewports.
- Improve dashboard feedback with visible loading state, refresh affordance state, and clear empty states for charts when there is no usage/ranking/trend data.
- Improve credentials management feedback with consistent status badges, table loading/empty rows, disabled action states while mutations are running, and clear one-time secret handling.
- Improve bucket management feedback with consistent ACL badges, table loading/empty rows, disabled action states while mutations are running, and friendly errors for invalid names, non-empty delete, missing bucket, and ACL failures.
- Preserve authentication behavior: protected API `401` still redirects to login, and UI changes must not display or persist `secret_key` outside the create success modal.

## Acceptance Criteria

- [x] `pkg/webadmin/ui/src` has cohesive, restrained styling for shell, nav, panels, tables, forms, modals, notices, loading rows, empty states, and badges without gradients/glass/oversized radii.
- [x] Dashboard shows a disabled/loading refresh state while fetching and renders clear empty states for capacity/ranking/trend charts when corresponding data is absent.
- [x] Credentials page shows visible loading and empty states, status badges for enabled/disabled credentials, disabled mutation buttons while save/toggle/delete is in progress, and the one-time `secret_key` only in the create modal.
- [x] Buckets page shows visible loading and empty states, ACL badges with public-read warning text, disabled mutation controls during create/ACL/delete, and friendly errors for known backend bucket errors.
- [x] Mobile view under 900px presents navigation as top links, keeps content padding reasonable, and allows wide tables to scroll horizontally instead of overflowing the viewport.
- [x] `npm run build` in `pkg/webadmin/ui` passes; `go build ./...` passes so embedded UI assets remain valid.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
