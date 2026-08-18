# General Webhook 部署指南

本项目的**唯一**推荐生产部署方式：通过 Dockerfile 构建镜像，并使用 Docker Compose 运行。不支持裸机二进制、systemd 或宿主机同步脚本。

## 部署原则

- 不配置 `ports`，不发布任何宿主机端口。
- 不接入公网反向代理或公网入口网络。
- 只有经过批准的生产者容器加入 `webhook-internal`。
- Webhook 通过独立的 `webhook-egress` 网络执行必要的出站 HTTP 请求。
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

`.env.example` 中的 source secret、action token 与 `ADMIN_TOKEN` 默认留空。编辑 `.env`，为管理接口设置至少 16 字节的独立随机 token，并只填写实际启用 source 所需的值；未使用的 source 应从 `configs/webhooks.yaml` 删除并重新构建镜像。不要复用 source token，也不要把 `.env` 加入镜像或版本库。

Deadman 心跳（可选）：`HEARTBEAT_URL` / `CF_HEARTBEAT_URL` 等全部留空时心跳模块不启动，无需改动镜像；启用时参考 README「心跳模块」一节。多机部署时为每台主机设置唯一的 `HOST_LABEL`（未设置则回退 `general-webhook`）。

正式发布必须使用唯一版本或提交哈希，不使用可漂移的 `latest`：

~~~dotenv
WEBHOOK_IMAGE=general-webhook:git-a1b2c3d
WEBHOOK_DATA_DIR=/data/webhook
~~~

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

## 4. 校验并构建

~~~bash
docker compose --env-file .env config -q
docker compose --env-file .env build --pull
~~~

`config -q` 只校验，不把展开后的 secret 输出到终端或 CI 日志。配置和 scripts 已烘焙进镜像；修改 `configs/webhooks.yaml` 或 `scripts/` 后必须使用新标签重新构建。

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

## 8. 更新、回滚与数据

升级前先做一致性备份。以下路径按标准值示例；若修改了 `WEBHOOK_DATA_DIR`，必须替换为对应的已核对路径：

~~~bash
docker compose --env-file .env stop webhook
sudo install -d -m 0700 /data/webhook-backups
backup_dir="/data/webhook-backups/$(date -u +%Y%m%dT%H%M%SZ)"
sudo mkdir -m 0700 "$backup_dir"
sudo cp -a /data/webhook/. "$backup_dir/"
docker compose --env-file .env start webhook
echo "backup: $backup_dir"
~~~

更新 `.env` 中的 `WEBHOOK_IMAGE` 为新且唯一的标签，然后构建、校验并替换：

~~~bash
docker compose --env-file .env config -q
docker compose --env-file .env build --pull
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
~~~

回滚时，把 `WEBHOOK_IMAGE` 改回仍保存在主机/镜像仓库中的上一唯一标签，再执行 `config -q` 和 `up -d --no-build --wait`。若新版本进行了旧程序不兼容的数据迁移，还必须在服务停止时恢复升级前的完整 SQLite 备份；这会丢弃备份之后收到的事件，应先评估影响。

从曾经把 action URL 展开后写入 SQLite 的版本升级时，新版本会用当前环境变量把可识别凭据替换回 `${secret:NAME}` 引用，但历史备份不会被自动修改。完成升级并核验后，应轮换 Telegram、飞书、钉钉等旧 token，并按保留策略安全处理旧备份。

不要执行 `docker compose down -v`。容器资源限制不限制 bind mount 的数据量，仍需监控磁盘、备份和执行 `store.retention` 保留策略。

关停语义：Compose `stop_grace_period`（默认 45s）内应用会停止接受新请求并停止认领新任务；超时取消的 in-flight 事件会回退为 `pending` 以便重启后继续处理，不会写成终态 `partial`。长耗时 `exec`（如 300s）不保证在单次关停窗口内跑完。
