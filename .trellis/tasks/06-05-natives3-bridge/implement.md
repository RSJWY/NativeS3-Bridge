# NativeS3-Bridge 集成执行计划（父任务）

> 本文件是**父任务级别**的执行编排：子任务推进顺序、集成评审门、最终验收命令。
> 各子任务的细粒度步骤在其自身 `implement.md`。父任务不直接写实现代码。

---

## 执行者须知（再次强调，不可违反）

- 按下表顺序激活子任务。每个子任务**完成并自检全绿后**才进入下一个。
- 子任务 6（前端）在子任务 3 完成后可与 4、5 并行。
- 任一子任务的 `prd/design/implement` 规格为冻结；执行中发现问题走 `research/change-request.md` 上报，**不得自行改规格**。
- 每完成一个子任务，运行其验证命令并对照其 Acceptance Criteria 逐项勾选。

---

## 子任务推进顺序与门禁

| 步骤 | 子任务 | 进入条件 | 完成门禁（全绿才算完成） |
|---|---|---|---|
| S1 | `06-05-db-foundation` | 无 | `go build ./...` 通过；三驱动各自启动能 AutoMigrate 建表 |
| S2 | `06-05-s3-core-objects` | S1 完成 | aws-cli 可 put/get/head/delete/list；磁盘可见原生文件 |
| S3 | `06-05-auth-quota` | S1,S2 完成 | 非法签名被拒；配额超限被拒；用量统计正确累加 |
| S4 | `06-05-multipart-metadata` | S2,S3 完成 | 100MB 分段上传落地单原生文件；元数据/标签可取回 |
| S5 | `06-05-presigned-hooks` | S3,S4 完成 | 预签名 URL 有效期内可用；事件回调送达 |
| S6 | `06-05-webadmin-ui` | S1,S3 完成（可与 S4/S5 并行） | 单密码登录；密钥CRUD；配额设置；ECharts 仪表盘渲染 |

---

## 集成评审门（所有子任务完成后，父任务执行）

### G1. 单文件构建
```bash
# 前端先构建嵌入
cd pkg/webadmin/ui && npm ci && npm run build && cd -
# 再构建二进制
go build -o natives3bridge ./cmd/natives3bridge
file natives3bridge   # 确认单个可执行文件
```

### G2. 三驱动启动验证
```bash
# SQLite
./natives3bridge --config configs/config.sqlite.yaml &   # 应自动建表并就绪
# MySQL（需本地实例）
./natives3bridge --config configs/config.mysql.yaml &
# PostgreSQL（需本地实例）
./natives3bridge --config configs/config.pg.yaml &
```

### G3. S3 端到端冒烟（scripts/smoke-test.sh）
```bash
export AWS_ACCESS_KEY_ID=<dashboard 创建的 key>
export AWS_SECRET_ACCESS_KEY=<对应 secret>
EP="--endpoint-url http://localhost:9000"
echo "hello" > /tmp/a.txt
aws $EP s3 cp /tmp/a.txt s3://test-bucket/dir/a.txt   # 上传
aws $EP s3 cp s3://test-bucket/dir/a.txt /tmp/b.txt    # 下载
diff /tmp/a.txt /tmp/b.txt                             # 内容一致
ls ./data/test-bucket/dir/a.txt                        # 磁盘原生文件存在
aws $EP s3 ls s3://test-bucket/dir/                     # 列举
# 分段上传大文件
head -c 120000000 /dev/urandom > /tmp/big.bin
aws $EP s3 cp /tmp/big.bin s3://test-bucket/big.bin     # 触发 multipart
ls -la ./data/test-bucket/big.bin                       # 单原生文件
ls ./data/.multipart                                    # 临时分片已清理（空）
aws $EP s3 rm s3://test-bucket/dir/a.txt                # 删除
```

### G4. 元数据 / 配额 / 鉴权
- 带 `--metadata author=jdoe` 上传，HEAD 取回应含 `x-amz-meta-author: jdoe`。
- 用错误 secret 调用，应返回 403 + 标准 XML。
- 把某 key 的 `quota_bytes` 设很小，上传超限文件应被拒。

### G5. 预签名 + 钩子
- 生成预签名 GET URL，用 `curl` 在有效期内下载成功；篡改签名或过期后返回 403。
- 配置一个本地 webhook 接收端，上传后应收到含 bucket/key/size 的 POST。

### G6. 管理界面
- 浏览器打开 `http://localhost:9001/`，单密码登录。
- 创建密钥 → 用该密钥跑通 G3。
- 设置配额 → 仪表盘显示用量。
- ECharts 三图（容量使用率、用量排行、请求趋势）正常渲染。

---

## 最终交付物清单

- [ ] 单文件二进制 `natives3bridge`（Windows/Linux 各一）
- [ ] `configs/config.example.yaml` + 三驱动示例
- [ ] `scripts/smoke-test.sh`
- [ ] `README.md`：部署、配置、aws-cli 接入、管理界面使用说明
- [ ] 各子任务 Acceptance Criteria 全绿
- [ ] 父任务 G1–G6 集成门全部通过

---

## 回滚点

- 每个子任务独立成 commit（或独立 PR），便于按子任务回滚。
- 前端构建产物与后端二进制分别可重建，互不阻塞。
