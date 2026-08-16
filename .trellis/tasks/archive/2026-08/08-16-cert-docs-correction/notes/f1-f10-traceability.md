# F1–F10 处理追溯（AC1 / V5）

本会话代码落盘记录。读序：prd 缺陷清单 → design §0 修正 → 本表。

## F1 备份六件套 offline root（multi-node-operations.md §6.1）
处理：删除原第 3 项（offline root），原第 4 项改写为部署 CA 准确表述（自签根、三重身份、丢失即全网重装），六件套变五件套；补一句说明早期清单含离线 root 但实现中从未存在，指向 §10.6。
design 裁决：§1.4（删一项而非改一项，避免与原第 4 项重复指向同一对文件）。
落点：docs/multi-node-operations.md §6.1。

## F2 docker-deployment.md 断链（:470）
处理：原承诺四项逐一处理——删除「离线 root CA」「intermediate 轮换」两项不存在的承诺（指向 §10.6 已知限制）；保留「节点证书撤销」「恢复演练和事故处理」并指向 §5/§8；新增证书全生命周期运维指向 §10。
落点：docs/docker-deployment.md §8 末尾段落。

## F3 pki.go:22 续期注释（已失效）
复核结论：design §0 已确认 08-16-cert-auto-renew 将该注释重写为 `Nodes renew via POST /renew over HTTPS mTLS before expiry`，与实现一致。本任务**仅复核，无需改动**。
附带修正：将 `(see design §3.3)` 这一悬空引用（指向任务目录，读代码的人跳不过去）改为指向 `docs/multi-node-operations.md §10.2`。
落点：pkg/panel/pki.go:23（仅注释，未改任何标识符）。

## F4 pki.go:32-34 offline root 注释
处理：改写 `CA` 结构体注释为实况陈述——部署 CA、pathlen:0 自签根、三重身份（客户端签发者/服务端签发者/唯一信任锚）、丢失即全网重装、指向 §10.6。
行号漂移：PRD 说 :26-27，实际 :32-34（design §0 已核）。
落点：pkg/panel/pki.go:32-38（仅注释）。

## F5 multi-node-operations.md:121 过期恢复无步骤
处理：§6.2 那句「Re-registration is required only for nodes whose certs were revoked or expired」末尾加指向 §10.4 的链接；§10.4 写完整七步恢复序列（含 D2 为何不做宽限期的论证）。
落点：docs/multi-node-operations.md §6.2 + §10.4。

## F6 全仓无证书运维章节
处理：新增 §10 Certificate lifecycle operations（英文六小节，结构见 design §1.3）—— §10.1 证书对照表、§10.2 自动续期、§10.3 到期巡检、§10.4 过期恢复、§10.5 服务端重签、§10.6 CA 已知限制 L1。
落点：docs/multi-node-operations.md §10。

## F7 docker-deployment.md:487 --force 警告无正向指引
处理：§9 安全边界的 `--force` 警告处补一句正向指引——服务端证书到期的正确做法见 §10.5，绝不要用 `--force`「续期」。
行号漂移：PRD 说 :480，实际内容在 :480（design §0 标 :487，以内容锚点定位为准）。
落点：docs/docker-deployment.md §9 末尾。

## F8 README 缺证书生命周期入口
处理：README 在证书 API 表格之后新增中文小节「证书生命周期」——90 天自动续期/阈值 30 天、服务端 825 天用 renew-server-cert 且不要用 --force、CA 3650 天到期需全网重装、链接到 §10；并补充 `GET /certs` 描述含剩余天数与四态状态。
落点：README.md 新增小节 + API 表格描述微调。

## F9 §8 恢复演练第 3 步复核
复核结论：design §2.4 已证经 08-16-panel-server-cert-renew 的 e2e 实证该表述仍然成立。本任务补一句：证书**已过期**的节点不在此列，走 §10.4。
落点：docs/multi-node-operations.md §8 第 3 步。

## F10 config/panel.go:55-56 offline root（PRD 清单外新发现）
处理：`PKIConfig` 注释同 F4 措辞纠偏——两处保持一致，便于日后 grep。点明部署 CA、无离线 root、见 §10.6。
来源：design §0 核实时发现，不在 PRD 原清单里；漏掉 AC2 直接失败。
落点：pkg/config/panel.go:55-58（仅注释）。
