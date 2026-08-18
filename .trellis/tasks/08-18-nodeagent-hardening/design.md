# NodeAgent 控制面连接加固 — 技术设计

> 对应 `prd.md`。只记录决策点。

## D1 证书校验的具体实现(R1)

新增 `validateIssuedCert(certPEM string, key crypto.Signer, caPEM []byte) error` 于 nodeagent 包:

1. `x509.ParseCertificate`(PEM decode 后第一张)。
2. 公钥匹配:`cert.PublicKey.(interface{ Equal(x crypto.PublicKey) bool }).Equal(key.Public())`(P-256 的 `*ecdsa.PublicKey` 有 Equal)。
3. 链验证:`cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})`;注册场景 pool 来自响应的 `ca_cert_pem`(先解析 CA,失败即拒),续期场景 pool 来自本地 `CAFile`。
4. 有效期:`now < NotAfter` 且 `NotAfter - now ≤ 10 年`。

注册与续期共用此函数,差别只在 CA 池来源。校验失败一律「不动旧文件 + 返回错误」。

## D2 原子写(R2)

```go
tmp := path + ".tmp"
os.WriteFile(tmp, data, perm) → f.Sync() → f.Close() → os.Rename(tmp, path)
```

同目录保证 rename 原子。`.bak` 备份:默认**做**(一行 `os.Rename(path, path+".bak")` 在写 tmp 之前;目标已存在才备份)。运维噪音可控(单份覆盖)。若实施中发现注册/续期之外还有 persistPEM 调用方(如 CA 落盘),统一走同一函数。

## D3 看门狗实现(R3)

- `handshake`:`ctx, cancel := context.WithTimeout(ctx, 30s)`,覆盖 hello 发送 + hello_ack 读取。
- serve 看门狗:独立 goroutine,ticker 每 `heartbeat_interval` 检查一次 `lastRecvAt`(每次 `ws.Read` 成功后原子更新);`time.Since(lastRecvAt) > max(3*interval, 60s)` → cancel serveCtx。看门狗随 serveCtx 退出,无泄漏。
- 选看门狗而非 per-Read deadline 的原因:coder/websocket 的读 deadline 需要每次 Read 重设,侵入循环;看门狗零侵入且语义就是「静默即死」。
- R3.3 心跳发送失败:调用统一的小方法 `abortServe(ws, cancel)`(Close + cancel),不复制粘贴。

## D4 续期断连的「计划内」标记(R4)

`connectAndServe` 当前只回 error。改为返回一个带类型的小结果或哨兵错误(`errRenewedReconnect`),`Run` 里 `errors.Is` 判定:计划内 → Info 日志、退避清零、立即进入下一轮;其他 → 现有 Warn + 退避。哨兵错误模式与本项目 error-handling 风格一致(见 spec backend/error-handling.md)。

## D5 task 校验与台账(R5)

- 校验顺序:先看 `task_id` 非空且 type 合法,再查幂等台账(命中时比对 type),最后执行。
- 回包:task_id 非空时回 failed 结果走现有 `sendMessage(task_result)` 路径,无新协议。
- 注意 `applied_tasks` 唯一索引:校验前置后,空 id 永不落库,存量库若已有 `""` 行(老版本产生)不影响——查询条件带 type 比对后天然 miss,正常执行。**不做**台账清理迁移(红线:无 schema/数据迁移)。

## D6 version 校验(R6)

`NegotiateVersion` 结果存进 client 连接态;分发时 `env.Version > negotiated`(或 > 本端 MaxSupported)→ Warn + continue。缺失 version 的帧按 v1 容忍(现网 panel 与 node 同版本,该分支只是防御)。不向 panel 回错误帧——协议未定义非 fatal 的运行期错误帧,回包反而制造兼容风险。

## D7 health 探针(R7)

实施第一步:对本地跑中的 node 执行 `curl -s http://127.0.0.1:<s3_port>/` 记录真实响应体(预期为 S3 XML Error)。探针判定:HTTP 状态码在 4xx/3xx 范围 **且** body 含 `<Error>` 与 `<Code>`。普通 http.server 返回 HTML 目录页,不满足,探针失败——达成区分目标。判定逻辑写成小函数以便单测(喂 httptest 假响应)。

## D8 不改的东西

- 不动 panel、不动 wire 字段、不动 `timeout_ms`(归 C4)、不动 `import_report` 大小(归 C4)。
- 不加新配置键(看门狗阈值从既有 heartbeat_interval 推导)。
