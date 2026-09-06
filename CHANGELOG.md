# Changelog

## 1.0.0 部署标签更新

本次修复对应 2026-09-06 审计（本地报告 REVIEW-2026-09-06.md）的 17 项正式发现。主分支提交必须通过完整 CI 后才更新 GHCR 镜像；本文记录代码与部署约定，实际发布状态以远端 CI 为准。

| 审计编号 | 修复 |
| --- | --- |
| HB-01 | 心跳运行期和配置错误不输出 URL 凭据；网络错误分类后进入 stdout/SQLite，不记录 path/query、token 头或响应体 |
| HB-02 | 健康采样要求 SQLite 真实写入提交、统计读取和本地 worker 就绪通过；只读故障报告 down |
| HB-03 | 默认使用 Kuma query `status=up/down`；Healthchecks 按解析后的路径追加 `/fail`，保留 query |
| HB-04 | 3xx 不算成功；Kuma 还要求 2xx 响应中的 `ok: true` |
| HB-05 | 最多 16 个独立目标 worker，每目标指标与有界采样/发送；SQL 错误落库有独立预算，停止时等待退出 |
| LP-01 | 拒绝统计固定为配置来源和有限原因，未知来源归 other；摘要少于 1 KiB |
| LP-02 | 拒绝原值不进入日志或心跳，提交 skipped 成功后才增加计数 |
| LP-03 | Prometheus 使用固定 reason 标签与正确转义，移除原始拒绝值和多行摘要注释 |
| LP-04 | 脱敏变量与可信执行身份分离；首次、重试和 replay 使用相同幂等键，不恢复已脱敏内容 |
| OUT-01 | HTTP 目标及重定向统一路径规则，拒绝编码穿越、分隔符、反斜杠与双重编码，保留普通百分号路径 |
| OUT-02 | exec 使用 context 取消、进程组终止和 250ms 管道等待上限；回归测试改用 Go helper，无 Python 依赖 |
| Q-01 | 派发前检查 max_retry_age，已到期的 pending/retrying 动作直接持久化 dead，不制造尝试；replay 重置期限 |
| Q-02 | 旧快照按主键分页处理，最多 128 行/1 MiB 一批，最大单条超过预算时单独处理 |
| IN-01 | 四层限流可配置，未认证请求不消费认证后全局额度，新增按 layer 的拒绝计数 |
| CFG-01 | 只展开原配置中的环境占位符一次，替换值中的美元符号保持字面值 |
| REL-01 | 同一提交全部 CI 成功后才能发布；Actions 固定 SHA，保留 provenance/SBOM，远端构建使用最新稳定 Go |
| DOC-01 | Healthchecks UUID URL 修正为 `https://hc-ping.com/<check-uuid>` |

另补齐时间戳窗口溢出防护、事务内租约/action 恢复、启动时 orphan running 状态恢复、SQLite WAL/FULL/250ms busy_timeout、数据库及 WAL/SHM 的 0600 权限，以及最多 256 项、每条表达式不超过 4 KiB 的正则编译缓存。

### 新增心跳审计

- `heartbeat_logs` 持久化 push 错误；通过管理鉴权访问 `GET /heartbeat/logs?limit=20`，limit 可设 1–100。
- 每条记录含 target、origin、mode、health、error、http_status、duration_ms、created_at；不含 URL path/query、token 头或响应体。
- 采样、发送各有 `T=heartbeat.timeout` 预算，失败落库另有 `min(T,1s)` 预算，一轮最多 `2T+min(T,1s)`；数据库不可用时回退 stdout 与 `log_failures` 计数。
- up 表示本地就绪；积压、dead、交付失败率和 worker 进展需独立监控。成功接受 down 也刷新投递成功时间，正常 Stop 不额外发送 down。

### 升级兼容

- 心跳缺省 mode 改为 `kuma`。已有 Healthchecks 配置应显式用 `healthchecks`，原有 `push` 继续作为 Healthchecks 别名；`json` 保持支持。内部 HTTP Kuma 需显式 `allow_http: true` 并重建自有镜像。
- 内置 GitHub relay 示例默认绑定 `[timestamp, delivery_id, body]`。升级时 relay 必须重签；维持旧 body-only 协议可显式设 `signed_headers: []`，其换头重放边界仍存在。
- policy 指标的 `value` 标签改为 `reason="allowlist_rejected"`；摘要不再包含被拒绝原值，计数重启清零。
- `max_retry_age` 同样约束尚未首次执行的积压动作；人工 replay 重新起算，done 动作保持不变。
- HTTP 的 Idempotency-Key、X-Webhook-Event-ID、X-Webhook-Attempt 由可信持久元数据设置，模板不能覆盖；消息变量继续按配置脱敏。
- exec 的等待有上限，但不保证终止已 setsid 脱离进程组的后代；脚本应自行清理并实现业务幂等。
- SQLite 在线备份应使用备份 API；停机备份复制完整目录，运行中只复制 `.db` 会遗漏 WAL 中的提交。

### 镜像与工具链

默认镜像为 `ghcr.io/metrogenes/general_webhook:1.0.0`，支持 linux/amd64、linux/arm64。`1.0.0` 是按部署要求持续更新的标签，与 `latest` 一起由主分支 CI 更新；另提供 `sha-<完整提交 SHA>` 定位代码，固定镜像产物使用 digest。**旧 Git `v1.0.0` 不移动。**

下载部署使用 `docker compose pull` 后 `up --no-build`。定制 configs/scripts 时使用自己的唯一标签 `build --pull`。官方镜像自带 revision；本地 `GENERAL_WEBHOOK_GIT_COMMIT` 仅作为 Compose 构建参数 VCS_REF。

CI 使用 Go `stable` + `check-latest`，Docker builder 使用 `golang:alpine` + pull；`go.mod` 的最低要求为 1.26.6，不锁定远端工具链。本次本机验证使用 Go 1.27.1，govulncheck 由当前 Go 安装最新版并记录版本。

### 验证

常规 Go 测试、race、vet 和二进制构建已通过，语句覆盖率为 71.4%。新增回归覆盖审计故障与身份/脱敏/过期边界。真实进程测试通过 `process_integration` build tag 执行，覆盖强制退出恢复、35 秒关停预算与心跳停止；本次通过耗时 45.096s，drain 为 35.028s。用 Go 1.27.1 编译的 govulncheck v1.7.0 源码扫描未发现已知漏洞。

`bash scripts/test-kuma.sh` 使用临时 Uptime Kuma 容器，已与 2.5.3 联调，自动清理容器与临时文件；它与真实进程测试均纳入 CI 发布门槛。详细验收命令见 [README](README.md#构建测试与发布)。
