# Quality Guidelines

> Code quality standards for backend development.

---

## Overview

本文只记录**在本仓库真实踩过、且下次很可能再犯**的模式。每条都附了当时的现场，
不写通用最佳实践。

---

## Forbidden Patterns

### F1. 测试 fixture 钉死日历日期，而被测代码读真实时钟

```go
// 反例(pkg/panel/adminapi_certs_test.go,2026-08-18 之前)
now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)   // fixture 钉在 8/16
cert := NodeCert{NotAfter: now.Add(-15 * 24 * time.Hour)}
// 但 handler 内部走的是 nowUTC() = time.Now()
// -> 8/18 这天 days_until_expiry 变成 -17,断言 want ~-15 直接挂
```

**判定规则**：fixture 里出现 `time.Date(20xx, ...)` 本身不是问题——只要那个 `now`
被**显式传进**被测函数（`certStatus(cert, now)`），测试就是自洽的。
只有当**被测代码自己读时钟**（`time.Now()` / `nowUTC()`）而 fixture 却钉死日历日时，
才是定时炸弹。

**正确写法**：

```go
now := time.Now().UTC()
// 偏移取在整天桶中间(15.5 天而不是 15 天),这样测试执行耗时不会让向上取整
// 在 -15/-16 之间抖动,断言可以是确定值而不是区间。
expiredAt := now.Add(-15*24*time.Hour - 12*time.Hour)
```

排查全仓库残留：`grep -rn "time\.Date(20" --include=*_test.go pkg/ cmd/`，
逐个确认那个 `now` 是否被传进了被测函数。

---

### F2. 用 `bool` 表达「可选的、默认为 true」的配置项

`bool` 无法区分「用户没写」与「用户显式写了 false」。`applyDefaults` 里写
`cfg.X = true` 会把用户的显式 false 静默吞掉。

```go
// 反例(pkg/config/config.go:271、panel.go:119,2026-08-18 之前)
c.WebAdmin.Ops.PublicHealthz = true    // 无条件覆盖,public_healthz: false 永不生效
```

**正确写法**：改 `*bool`，配一个 nil 安全的访问器，读取点一律走访问器：

```go
PublicHealthz *bool `yaml:"public_healthz"`

func (o OpsConfig) HealthzPublic() bool {
    return o.PublicHealthz == nil || *o.PublicHealthz   // nil = 未配置 = 历史默认
}
```

**关键陷阱：改完第一处不等于修好了。** 同一个字段往往有**第二处覆盖点**藏在下游：

```go
// pkg/webadmin/ops.go 的 NewOpsHandler 里还有一段
if !cfg.PublicHealthz && !cfg.PublicReadyz && !cfg.PublicMetrics && cfg.MetricsToken == "" {
    cfg.PublicHealthz = true    // 「看起来像零值就恢复默认」的兜底
}
```

而「只写 `public_healthz: false`、其余不写」恰好命中这个形状，用户的显式 false
会被二次吞掉。改这类默认值语义时，**必须 `grep -rn "\.FieldName"` 把所有写入点
过一遍**，光改 `applyDefaults` 是不够的。

---

### F3. 文件权限检查用「宽于 0640」做字面位比较

```go
// 反例:把属主位和执行位也算进了暴露面
if mode &^ 0o640 != 0 { warn() }
// chmod 0700 (rwx------,别人根本读不到,比 0640 更严) -> 0700 &^ 0640 = 0100 -> 误报
```

**正确写法**：只看真正造成暴露的位——组可写 + 其他用户可读写。属主位与执行位
不影响「谁能读到内容」，不纳入判定：

```go
const nodeConfigLeakBits os.FileMode = 0o026   // group-w, other-r, other-w
if mode & nodeConfigLeakBits != 0 { warn() }
```

---

## Required Patterns

### R1. 占位值黑名单用「特征词包含匹配」，不用「逐串精确匹配」

精确匹配挡不住变体。`configs/panel.example.yaml` 的
`replace-with-a-random-32-byte-secret-value` 只比黑名单条目多了 `-value` 后缀，
长度又达标，于是占位密钥一路通过校验直达生产——而它就写在公开仓库里。

```go
// 特征词包含匹配(小写),覆盖未来的变体
var placeholderSecretMarkers = []string{
    "replace-with", "change-me", "example", "todo", "placeholder", "your-secret", ...
}
// 精确串保留作兜底:不含特征词的历史示例值也要继续被拒
```

配套要求：**报错必须自解释并直接给出生成命令**。只说「不合法」会让运维把占位值
改得更长，而不是换成随机值：

```
webadmin.session_secret looks like an example placeholder (matched "..."); ...
Replace it with a random value: `openssl rand -base64 32`
```

加严这类校验前，**先 grep 全仓库的示例配置、测试、安装脚本**，确认没有合法值被
误杀（本仓库安装脚本用 `openssl rand -hex 32`，hex/base64 字符集都不会命中特征词）。

---

### R2. 启动期告警放在日志配置完成之后

`LoadNode` / `LoadPanel` 在 `setupSlog` 之前执行，此时 `slog.Warn` 只会落到默认
stderr，进不了配置好的日志文件/目录。

因此 `pkg/config` 保持**零日志依赖**：校验函数只做判定并返回结果
（如 `InsecureNodeConfigMode(path) os.FileMode`），由 `cmd/*/main.go` 在
`setupSlog` 之后决定怎么告警。

---

### R3. 运行时字符串用英文，注释可用中文

全仓库的 `fmt.Errorf` / `slog.*` 消息**没有一条中文**（可 grep 验证）。
错误与日志面向运维、可能被 grep/告警规则匹配，一律英文；代码注释用中文说明
「为什么」。

---

## Testing Requirements

- 校验加严类改动必须同时有**两个方向**的测试：命中被拒（安全性）**和**合法值通过
  （回归保护）。只测前者会让下一个人无法察觉误杀。
- 涉及「默认值语义」的改动，三种情况都要覆盖：显式 true / 显式 false / 完全不写。
- 修 bug 时把**误报案例本身**钉进测试表（如权限检查的 `{0o700, false}`），
  否则下一次重构很容易把同样的边界再算错。

---

## Code Review Checklist

- [ ] 改默认值语义时，是否 grep 过该字段的**所有**写入点（不止 `applyDefaults`）？
- [ ] 新增/加严校验，是否 grep 过仓库内所有配置、脚本、测试确认无误杀？
- [ ] 新增测试是否依赖真实日历日？被测代码读时钟吗？
- [ ] 报错信息是否给出了可执行的修复动作，而不只是「不合法」？
- [ ] 运行时字符串是否为英文？
