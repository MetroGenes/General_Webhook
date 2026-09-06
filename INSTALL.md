# General Webhook 部署指南

推荐使用 Docker Compose 运行。默认拉取 `ghcr.io/metrogenes/general_webhook:1.0.0`，支持 amd64/arm64；需要定制配置或脚本时，通过 Dockerfile 构建自己的唯一标签镜像。

## 部署原则

- 不配置 `ports`，不发布任何宿主机端口。
- 不接入公网反向代理或公网入口网络。
- 只有经过批准的生产者容器加入 `webhook-internal`。
- Webhook 通过独立的 `webhook-egress` 网络执行 HTTP action 和主动心跳请求。
- 镜像内置配置和脚本；修改后必须重新构建镜像。
- 容器固定以 10001:10001 非 root 用户运行，根文件系统只读。
- 默认限制为 0.5 CPU、256 MiB RAM+swap 总量、64 个 PID 和有界日志。

## 1. 准备环境

要求仍受维护的 Docker Engine 和 Docker Compose plugin v2 或更高版本；Compose 必须支持 `up --wait --wait-timeout` 与 bind 的 `create_host_path: false`。先确认版本，再复制环境文件并限制权限：

~~~bash
docker version
docker compose version
cp .env.example .env
chmod 600 .env
~~~

若当前 Compose 不识别这些参数或配置字段，应先升级 Compose，不要通过删除健康等待或自动建数据目录来绕过。

`.env.example` 中的 source secret、action token 与 `ADMIN_TOKEN` 默认留空。编辑 `.env`，为管理接口设置至少 16 字节的独立随机 token，并填写镜像内所有已启用 source/action 所需的值。官方示例包含 `github`、`custom`、`bt`、`dingtalk-demo`；需删减时修改 `configs/webhooks.yaml` 并按第 4 节构建自己的镜像。凭据含 `$` 时在 `.env` 用单引号包裹，避免 Compose 提前插值。不要复用管理和 source token，也不要把 `.env` 加入镜像或版本库。

主动心跳（可选）：`HEARTBEAT_URL` / `CF_HEARTBEAT_URL` 全部留空时模块不启动。默认目标是 Uptime Kuma Push，复制 Kuma 创建监控项后生成的 `https://.../api/push/<token>?status=up&msg=OK&ping=` 到 `HEARTBEAT_URL`。每个实例使用独立监控项和唯一 `HOST_LABEL`（最多 128 字节，缺省为 `general-webhook`）。

`.env` 的默认镜像设置：

~~~dotenv
WEBHOOK_IMAGE=ghcr.io/metrogenes/general_webhook:1.0.0
WEBHOOK_DATA_DIR=/data/webhook
~~~

主分支通过全部 CI 后更新 `1.0.0` 和 `latest`，并发布 `sha-<完整提交 SHA>`。`1.0.0` 是按部署约定可更新的标签；按提交标签可定位代码版本，完全固定产物应使用镜像 digest。旧 Git `v1.0.0` 标签保留原提交。升级前记录正在运行的镜像 digest 或已验证提交标签，便于回滚。

内置 GitHub relay 示例已启用 `signed_headers: [timestamp, delivery_id, body]`。relay 应先验证 GitHub 上游签名，再用本服务共享 secret 对 `timestamp + "\n" + delivery_id + "\n" + 原始 body` 重签。升级旧 relay 时必须同步改签名；需要维持旧 body-only 协议时，在自有配置中显式设 `signed_headers: []` 并重建，该协议仍不绑定时间戳和 delivery ID。

下文命令使用标准的 `/data/webhook`、`10001:10001` 和 `webhook-internal`。若覆盖这些值，必须把所有目录准备、备份、网络创建及生产者 Compose 示例同步改成实际值，不能只修改 `.env`。

## 2. 准备数据目录

容器默认固定使用 UID/GID `10001:10001`。Compose 使用 `create_host_path: false`，不会以 root 身份静默代建目录；启动前必须按 `.env` 的实际路径显式准备（标准路径如下）：

~~~bash
sudo install -d -o 10001 -g 10001 -m 0700 /data/webhook
~~~

不要把整个项目目录设为容器可写。若从旧的 root/其它 UID 容器迁移，先停止旧实例并备份数据库，再确认路径无误后修正已有文件所有权：

~~~bash
sudo chown -R 10001:10001 /data/webhook
sudo chmod 0700 /data/webhook
~~~

若确实覆盖 `WEBHOOK_UID`/`WEBHOOK_GID`，Dockerfile 构建参数、Compose 运行用户和数据目录所有权必须使用同一组数值。

## 3. 创建受限入口网络

入口网络由管理员预创建，并且必须验证为 Docker internal 网络。以下命令会拒绝复用同名的普通 bridge，不会自动删除或替换已有网络：

~~~bash
network_name=webhook-internal
if docker network inspect "$network_name" >/dev/null 2>&1; then
  test "$(docker network inspect --format '{{.Internal}}' "$network_name")" = true || {
    echo "existing network is not internal: $network_name" >&2
    exit 1
  }
else
  docker network create --driver bridge --internal "$network_name"
fi
~~~

该网络只用于 Webhook 与批准的生产者容器通信。`network_name` 必须与 `.env` 的 `WEBHOOK_INTERNAL_NETWORK` 一致；若没有明确的多实例需求，保持默认名称。Webhook 同时加入由本 Compose 管理的 `webhook-egress` 网络，以便发送 Telegram、飞书等出站请求。egress 是出站通道，不会发布宿主端口，也不是公网入站网络。

不要将 `webhook-internal` 改成普通公共 bridge，也不要添加 `0.0.0.0:8080:8080`、`network_mode: host`、公网入口网络或公网反向代理。

## 4. 校验并拉取镜像

~~~bash
docker compose --env-file .env config -q
docker compose --env-file .env pull webhook
~~~

`config -q` 只校验，不把展开后的 secret 输出到终端或 CI 日志。使用官方镜像无需本机安装 Go，下一步用 `up --no-build` 启动。

配置和 scripts 已烘焙进镜像。定制 `configs/webhooks.yaml`、`scripts/`、UID/GID，或为内部 HTTP Kuma 开启 `allow_http: true` 时，先将 `.env` 改为自己的唯一标签：

~~~dotenv
WEBHOOK_IMAGE=general-webhook:local-20260906-01
# 可选：填当前完整提交 SHA，仅用于本地构建的 VCS_REF
GENERAL_WEBHOOK_GIT_COMMIT=
~~~

然后构建并执行第 5 节启动命令：

~~~bash
docker compose --env-file .env config -q
docker compose --env-file .env build --pull
~~~

每次定制构建都使用新标签。Docker builder 使用 `golang:alpine` 并拉取最新基础镜像；远端 CI 使用 Go `stable` + `check-latest`，`go.mod` 的 `1.26.6` 只是最低要求。本次本机测试工具链为 Go `1.27.1`。官方镜像自带 revision；Compose 的 `GENERAL_WEBHOOK_GIT_COMMIT` 只传给 `build.args.VCS_REF`，不覆盖官方镜像的运行环境。

## 5. 启动并等待健康

~~~bash
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
docker compose --env-file .env ps
docker compose --env-file .env logs --tail=100 webhook
~~~

Compose 中的 expose 只是镜像/容器网络元数据，不会发布宿主端口。可以确认 HostPort 为空：

~~~bash
docker inspect --format '{{json .HostConfig.PortBindings}}' \
  "$(docker compose --env-file .env ps -q webhook)"
~~~

结果应为 `{}` 或 `null`，且 `docker compose ps` 的 PORTS 列不应出现 `0.0.0.0`、`[::]` 或宿主端口。

## 6. 接入批准的生产者容器

其它 Compose 项目只把确实需要调用 Webhook 的服务加入入口网络：

~~~yaml
services:
  monitor:
    networks:
      - webhook-internal
    environment:
      WEBHOOK_URL: http://general-webhook:8080/webhook/custom
      WEBHOOK_TOKEN: ${WEBHOOK_TOKEN:?set the custom source token}

networks:
  webhook-internal:
    external: true
    name: webhook-internal
~~~

生产者必须把 `WEBHOOK_TOKEN` 作为 `X-Webhook-Token` 请求头发送，其值与 Webhook 侧的 `CUSTOM_TOKEN` 一致；token 不得放入 URL 查询参数。实际应用应按自身能力安全注入该值。

不要把数据库、缓存、普通业务容器或不受信任任务批量加入该网络。Docker internal 网络限制外部路由，但不是面向 Docker 管理员的 ACL；拥有 Docker daemon 权限的人仍可把任意容器接入，因此 daemon 权限本身必须按 root 等级控制。

默认部署无法被 GitHub、宝塔等公网来源直接调用。若确需接收公网事件，应使用独立、最小化的 relay/agent 完成公网接收、来源校验和限流，再由批准的内部连接转发，并单独完成安全评审；不要直接给本服务添加公网端口。

## 7. 资源与健康检查

默认资源边界：

| 项目 | 默认值 |
| --- | --- |
| CPU | 0.50 |
| 内存硬限制 | 256 MiB |
| RAM + swap 总上限 | 256 MiB（默认不提供额外 swap） |
| 内存预留 | 64 MiB |
| Go 内存软限制 | 192 MiB |
| PID | 64 |
| 打开文件数 | soft 2048 / hard 4096 |
| /tmp | 16 MiB，noexec/nosuid/nodev |
| 日志 | 10 MiB × 5 |

这些值适合低流量监控类 Webhook。`WEBHOOK_MEMORY_SWAP_LIMIT` 不得小于 `WEBHOOK_MEMORY_LIMIT`。若触发大型 exec，应优先把重任务迁移到专用 worker，而不是无限提高 Webhook 容器限额。

容器还启用固定非 root 用户、只读根文件系统、`cap_drop: ALL` 和 `no-new-privileges`。exec 脚本只能写入 `/app/data` 或受限 `/tmp`；不要为了脚本方便而挂载 Docker socket、宿主根目录或任意可写目录。

应用会执行 action 级 `url_allowlist`、DNS/IP 和重定向校验；`webhook-egress` 本身只隔离网络成员，不是网络层域名防火墙。需要独立于应用配置的强制出站策略时，应在宿主机防火墙或专用出站代理实现。

健康检查访问 `/readyz` 并验证 worker 存活及 SQLite 可写。`restart: unless-stopped` 只会在进程退出时重启；单纯 `unhealthy` 不会自动重启，应由监控系统告警。容器停止宽限期为 45 秒，用于完成应用最长 35 秒的关停流程。Prometheus 抓取 `/metrics` 时必须携带管理 Bearer token，并至少对 `dead/partial/error > 0`、最老未完成事件超时和 5 分钟 action 失败率设置告警。

Kuma 的 `up` 表示本地就绪检查通过，动作交付、队列积压和 worker 进展需另设告警。心跳目标最多 16 个，各自独立调度；健康采样和发送各最多 `T=heartbeat.timeout`，失败日志另有 `min(T,1s)` SQL 预算，一轮最多 `2T+min(T,1s)`。宽限应覆盖 interval、最坏单轮耗时和预期重启时间，正常停止不会主动发送 down。

内部 HTTP Kuma 需要受控网络互通，并在镜像配置中显式设置 `allow_http: true` 后重建自有镜像；仅修改 `.env` URL 不会开启 HTTP。继续使用 Healthchecks 时设 `mode: healthchecks`（旧 `push` 是其别名），UUID URL 为 `https://hc-ping.com/<check-uuid>`；JSON 接收器使用 `mode: json`。省略 mode 的旧配置升级后会改用 Kuma。

失败 push 同时写 stdout 和 `heartbeat_logs`。从受控运维网络携带管理 token 访问 `GET /heartbeat/logs?limit=20` 可查看最近错误，limit 范围 1–100。记录包含目标名称、origin、协议、健康值、错误类别、HTTP 状态和耗时，不包含 URL path/query、token 头或响应体。数据库不可写时只能回退 stdout，并增加 `general_webhook_heartbeat_target_log_failures_total`；也应监控每目标 push failures/last success。

入口 `server.rate_limit` 可配置鉴权前 IP、认证后全局、source/IP 与管理 IP 四层容量；未认证流量不消费认证后的全局额度。通过 `general_webhook_rate_limit_rejections_total{layer}` 观察 429，并确保生产者执行退避重投。默认值与完整指标见 [README](README.md#运维接口)。

## 8. 更新、回滚与数据

SQLite 使用 WAL、FULL 同步和 250ms 锁等待；数据库及 `-wal` / `-shm` 文件权限为 `0600`。升级前先做一致性备份：停机后复制整个数据目录，或使用 SQLite 备份 API，运行中单独复制 `.db` 不足以保存 WAL 中已提交的数据。以下路径按标准值示例；若修改了 `WEBHOOK_DATA_DIR`，必须替换为对应的已核对路径：

~~~bash
docker compose --env-file .env stop webhook
sudo install -d -m 0700 /data/webhook-backups
backup_dir="/data/webhook-backups/$(date -u +%Y%m%dT%H%M%SZ)"
sudo mkdir -m 0700 "$backup_dir"
sudo cp -a /data/webhook/. "$backup_dir/"
docker compose --env-file .env start webhook
echo "backup: $backup_dir"
~~~

使用官方部署标签更新时，校验、重新拉取并替换容器：

~~~bash
docker compose --env-file .env config -q
docker compose --env-file .env pull webhook
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
~~~

自有配置/脚本的更新按第 4 节使用新标签构建，然后 `up --no-build`。回滚时，把 `WEBHOOK_IMAGE` 改为记录的上一 digest 或已验证提交标签，再执行 `config -q`、必要的 `pull` 和 `up -d --no-build --wait`；不能通过再次填写可更新的 `1.0.0` 找回旧产物。若新版本进行了旧程序不兼容的数据迁移，还必须在服务停止时恢复升级前的完整 SQLite 备份；这会丢弃备份之后收到的事件，应先评估影响。

从曾经把 action URL 展开后写入 SQLite 的版本升级时，新版本会用当前环境变量把可识别凭据替换回 `${secret:NAME}` 引用。该迁移按稳定主键分页，每批最多 128 行和 1 MiB；超过预算的最大单条快照需单独载入。历史备份不会被自动修改，完成升级并核验后应按实际情况轮换旧凭据和处理旧备份。

本次升级还需注意：policy 指标从原 `value` 标签改为固定 `reason="allowlist_rejected"`；`max_retry_age` 会在派发前拒绝已到期的 pending/retrying 动作，包括尚未首试的积压，人工 replay 重新起算；HTTP 三个执行身份头由持久元数据设置，消息变量的脱敏结果在重试/replay 中保持不变。完整修复清单见 [CHANGELOG.md](CHANGELOG.md)。

不要执行 `docker compose down -v`。容器资源限制不限制 bind mount 的数据量，仍需监控磁盘、备份和执行 `store.retention` 保留策略。

关停语义：Compose `stop_grace_period`（默认 45s）内应用会停止接受新请求并停止认领新任务；超时取消的 in-flight 事件会回退为 `pending` 以便重启后继续处理，不会写成终态 `partial`。长耗时 `exec`（如 300s）不保证在单次关停窗口内跑完。

exec 取消或退出后，额外输出管道等待最多 250ms；已用 `setsid` 脱离进程组的后代仍需脚本或容器生命周期负责清理。常规测试、真实进程 kill/restart 与关停测试、Kuma 容器测试的命令见 [README](README.md#构建测试与发布)。
