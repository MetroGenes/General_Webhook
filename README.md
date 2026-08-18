# General Webhook

General Webhook 是一个用 Go 编写的通用 webhook 网关。它提供统一的 HTTP 入口，负责接收事件、校验来源、从 JSON payload 中抽取变量、按规则过滤事件，并异步执行配置好的动作。

项目的标准部署面向 Docker 内部的监控与应用事件，例如 Fluent Bit、容器状态监测器和同机业务容器。生产环境通过 Dockerfile 构建镜像、使用 Docker Compose 运行，默认不发布宿主机端口，也不接入公网反向代理。GitHub、宝塔等公网来源若确需接入，应先经过独立的内部 relay/agent，并单独评审入口风险。

## 功能概览

- 容器内统一入口：`POST /webhook/{source}`，`source` 对应配置里的事件源名称。
- 来源鉴权：支持 GitHub 风格 HMAC-SHA256、静态 token，也可以显式配置为 `none`。
- 变量抽取：使用 `gjson` 从 JSON payload 抽取变量，配置里可写 `$.a.b`。
- 规则过滤：对抽取变量做正则匹配，所有规则命中才执行动作。
- 异步处理：请求写入 SQLite outbox（`pending`）后返回 `202 Accepted`，worker 认领并执行动作；进程重启可继续处理未完成任务。
- 动作类型：通用 HTTP 请求（支持 Telegram/飞书/钉钉/自定义服务）+ 本地命令执行。
- 审计记录：SQLite 保存事件、每个 action 的持久重试状态、耐久化抽取变量和已脱敏动作日志；原始 payload 是否长期保留由 `store.retain_payload` 控制。
- 结构化日志：使用标准库 `log/slog` 输出 JSON 日志，`event_id` 同时作为 `trace_id`。
- 死链心跳：进程周期性向外部监控端点（Healthchecks.io / Uptime Kuma / Cloudflare Pages）主动 POST 存活与队列健康信号，解决"无人监控告警中枢"的缺口；不配置 target 时模块不启动。

## 工作流程

```text
POST /webhook/github
  -> 校验 source 是否存在
  -> 读取并限制 body 大小，最大 1 MiB（进程级并发与在途字节预算）
  -> 按 source.auth 校验签名或 token
  -> 生成 event_id / trace_id
  -> 以 pending 状态持久化到 SQLite outbox
  -> 唤醒 worker（内存通道只作为唤醒信号）
  -> 返回 202 Accepted

worker 异步处理
  -> 按 extract 抽取变量
  -> 按 rules 过滤事件
  -> 命中后，同一事务持久化 vars_json 并初始化 action 状态
  -> 逐个执行尚未成功的 actions
  -> 失败动作写入持久 next_attempt_at，按指数退避 + full jitter 重试
  -> Retry-After 到期前不占用 worker；超过次数/期限进入 dead letter
  -> 更新事件状态并写入 action_logs
```

事件状态：

| 状态 | 含义 |
| --- | --- |
| `received` / `rejected` | 旧版本兼容状态；启动迁移时转为 `pending` |
| `pending` | 已持久化，等待 worker 认领 |
| `processing` | worker 已认领，执行中 |
| `retrying` | 至少一个 action 等待持久延迟重试 |
| `done` | 所有动作执行成功 |
| `dead` | 至少一个动作达到重试次数/期限或发生永久错误，可人工 replay |
| `skipped` | 规则未命中，未执行动作 |

旧数据库中的 `partial` / `error` 只作为迁移兼容状态保留。

## 项目结构

```text
.
├── cmd/server/main.go              # 程序入口、HTTP server、优雅关停
├── internal/
│   ├── auth/                       # HMAC / token 鉴权
│   ├── config/                     # YAML 配置、环境变量注入、校验
│   ├── handler/                    # http / exec 动作处理器
│   ├── heartbeat/                  # 外部死链心跳推送 (Healthchecks/CF Pages 等)
│   ├── logger/                     # slog JSON 日志与 trace_id
│   ├── parser/                     # gjson 变量抽取
│   ├── queue/                      # SQLite outbox worker、唤醒、租约与重试
│   ├── router/                     # 正则规则匹配
│   ├── server/                     # HTTP 路由与 webhook 接入层
│   └── store/                      # SQLite outbox、审计、保留与 payload 清理
├── configs/webhooks.yaml           # 示例配置
├── deploy/                         # 部署说明（指向 Dockerfile + Compose）
├── scripts/run.sh                  # exec 动作示例脚本
├── INSTALL.md                      # Docker Compose 正式部署指南
├── .env.example                    # 部署参数、secret 与资源限制示例
├── Dockerfile
├── docker-compose.yaml
├── go.mod
└── go.sum
```

主要依赖：

| 依赖 | 用途 |
| --- | --- |
| `log/slog` | JSON 结构化日志 |
| `github.com/tidwall/gjson` | JSON payload 变量抽取 |
| `gopkg.in/yaml.v3` | YAML 配置解析 |
| `modernc.org/sqlite` | 纯 Go SQLite 驱动，无需 CGO |

## 快速开始

### 标准生产部署（Dockerfile + Compose）

完整步骤和运维说明见 [INSTALL.md](INSTALL.md)。标准部署只挂载 SQLite 数据目录；`configs/` 与 `scripts/` 会在构建时烘焙进镜像，修改后必须重新构建。以下快速命令使用标准的 `/data/webhook` 与 `webhook-internal`；如需覆盖路径或网络名，应按完整指南同步替换所有相关命令和生产者配置。

```bash
cp .env.example .env
chmod 600 .env
# 编辑 .env：设置唯一镜像标签和已启用 source 所需的 secret；快速示例保持标准路径/网络名

sudo install -d -o 10001 -g 10001 -m 0700 /data/webhook
if docker network inspect webhook-internal >/dev/null 2>&1; then
  test "$(docker network inspect --format '{{.Internal}}' webhook-internal)" = true || {
    echo "existing webhook-internal network is not internal" >&2
    exit 1
  }
else
  docker network create --driver bridge --internal webhook-internal
fi

docker compose --env-file .env config -q
docker compose --env-file .env build --pull
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
```

默认部署边界：

| 项目 | 默认行为 |
| --- | --- |
| 宿主机入口 | 没有 `ports`，不会发布 `8080` |
| `webhook-internal` | 管理员预创建的 internal 网络；只允许批准的生产者容器加入 |
| `webhook-egress` | Webhook 专用出站网络，用于访问 Telegram、飞书等目标；不是公网入站网络 |
| 文件系统 | 固定 `10001:10001` 非 root、只读根文件系统，仅 `/app/data` 和受限 `/tmp` 可写 |
| 资源 | 0.50 CPU、256 MiB RAM+swap 总量、64 MiB 内存预留、64 PID、16 MiB `/tmp`、日志 10 MiB × 5 |

`EXPOSE 8080` 和 Compose 的 `expose` 只是容器网络元数据，不会创建 HostPort。可验证宿主机没有端口绑定：

```bash
docker inspect "$(docker compose --env-file .env ps -q webhook)" \
  --format '{{json .HostConfig.PortBindings}}'
```

批准的其它 Compose 服务通过同一个 external internal 网络调用：

```yaml
services:
  monitor:
    image: your-monitor:local
    networks:
      - webhook-internal
    environment:
      WEBHOOK_URL: http://general-webhook:8080/webhook/custom
      WEBHOOK_TOKEN: ${WEBHOOK_TOKEN:?set the custom source token}

networks:
  webhook-internal:
    external: true
    name: webhook-internal
```

调用方必须把 `WEBHOOK_TOKEN` 作为 `X-Webhook-Token` 请求头发送，其值应与 Webhook 侧的 `CUSTOM_TOKEN` 一致；不得放进 URL 查询参数。不要把数据库、缓存、普通业务服务或不受信任任务批量接入该网络。Docker 网络成员资格由拥有 Docker daemon 权限的管理员控制；它不能防御已有 daemon 权限的用户。

### 本地开发调试（非生产部署）

项目要求 Go `1.26.6`（CI 与构建镜像使用；`go.mod` 的 `go` 指令为最低语言版本 `1.26.5`，本地用 1.26.5+ 均可）。先删除示例配置中不使用的 source，或为其提供所需环境变量。开发进程也必须只监听 loopback；复制配置并修改监听地址后再运行：

```bash
export GITHUB_SECRET='your_github_secret'
export FEISHU_BOT='https://open.feishu.cn/open-apis/bot/v2/hook/xxx'
export ADMIN_TOKEN='local_admin_token_123456'
export CUSTOM_TOKEN='local_custom_token_123456'
export BT_TOKEN='local_baota_token_123456'
export TG_BOT_TOKEN='123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11'
export TG_CHAT_ID='123456789'
export DINGTALK_TOKEN='dingtalk_access_token'
export DINGTALK_WEBHOOK_TOKEN='dingtalk_inbound_token'

cp configs/webhooks.yaml /tmp/general-webhook-dev.yaml
sed -i '0,/addr: ":8080"/s//addr: "127.0.0.1:8080"/' /tmp/general-webhook-dev.yaml
sed -i 's#path: "data/webhook.db"#path: "/tmp/general-webhook-dev.db"#' /tmp/general-webhook-dev.yaml
sed -i 's#/opt/general-webhook/scripts/run.sh#./scripts/run.sh#' /tmp/general-webhook-dev.yaml
go run ./cmd/server -config /tmp/general-webhook-dev.yaml
```

该方式不具备 Compose 的网络与资源边界，只用于隔离的开发环境；不要直接用监听 `:8080` 的原始示例配置启动开发服务。

```bash
curl http://127.0.0.1:8080/readyz
curl -X POST 'http://127.0.0.1:8080/webhook/custom' \
  -H 'Content-Type: application/json' \
  -H 'X-Webhook-Token: local_custom_token_123456' \
  -d '{"job":"deploy-api"}'
```

成功时返回 `202 Accepted`：

```json
{"event_id":"20260618T022345.120-a1b2c3d4e5f6","status":"accepted"}
```

## 配置说明

完整示例见 `configs/webhooks.yaml`。新增接入时，通常只需要在 `sources` 下增加一个 source。

**核心理念**：action 只有 `http` 和 `exec` 两种类型，`http` 支持完全自定义 URL/Headers/Body，通过模板插值适配任意 webhook API（Telegram、飞书、钉钉、Discord、Slack、自建服务等），零代码扩展。

```yaml
server:
  addr: ":8080"
  trusted_proxies: []

admin:
  header: Authorization
  token: ${ADMIN_TOKEN}

log:
  level: "info"

store:
  path: "data/webhook.db"
  retention: 720h
  retain_payload: false

queue:
  workers: 4
  buffer: 256
  max_retries: 2
  retry_base: 1s
  max_retry_delay: 15m
  max_retry_age: 24h

heartbeat:
  interval: 60s
  timeout: 10s
  host: ${HOST_LABEL}
  targets:
    - name: healthchecks
      url: ${HEARTBEAT_URL}     # Healthchecks.io push URL (env, 含一次性 token)
      token: ${HEARTBEAT_TOKEN} # 可选, 作为 X-Heartbeat-Token 头发送
      mode: push                # push(默认) | json
    - name: cf-pages
      url: ${CF_HEARTBEAT_URL}  # Cloudflare Pages 心跳接收器
      token: ${CF_HEARTBEAT_TOKEN}
      mode: json

sources:
  # 示例 1: 内部 relay 转发的 GitHub 事件 -> 飞书群机器人
  - name: github
    auth:
      type: hmac
      header: X-Hub-Signature-256
      secret: ${GITHUB_SECRET}
    extract:
      event: "$.action"
      branch: "$.ref"
      repo: "$.repository.full_name"
    rules:
      - var: branch
        regex: "^refs/heads/(main|master)$"
    actions:
      - type: http
        url: ${secret:FEISHU_BOT}
        method: POST
        headers:
          Content-Type: application/json
        body: |
          {
            "msg_type": "text",
            "content": {"text": "🚀 ${repo} pushed to ${branch}"}
          }

  # 示例 2: 内部 relay 转发的宝塔面板告警 -> Telegram bot
  - name: bt
    auth:
      type: token
      header: X-Webhook-Token
      secret: ${BT_TOKEN}
    extract:
      title: "$.title"
      msg: "$.msg"
      type: "$.type"
    rules: []
    actions:
      - type: http
        url: "https://api.telegram.org/bot${secret:TG_BOT_TOKEN}/sendMessage"
        method: POST
        headers:
          Content-Type: application/json
        body: |
          {
            "chat_id": "${secret:TG_CHAT_ID}",
            "text": "🛎 宝塔告警 [${type}]\n${title}\n${msg}"
          }

  # 示例 3: 自定义来源 -> 执行本地脚本
  - name: custom
    auth:
      type: token
      header: X-Webhook-Token
      secret: ${CUSTOM_TOKEN}
    extract:
      job: "$.job"
    rules: []
    actions:
      - type: exec
        command: "/opt/general-webhook/scripts/run.sh"
        args: ["${job}"]
        timeout: 300s
```

### 顶层配置

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `server.addr` | `:8080` | 容器内 HTTP 监听地址；不表示宿主机端口已发布 |
| `server.trusted_proxies` | `[]` | 可代表客户端传递转发头的可信代理 IP/CIDR；标准部署无入站反代，应保持为空 |
| `server.max_in_flight` | `16` | 鉴权前全局并发请求上限 |
| `server.max_in_flight_bytes` | `32MiB` | 鉴权前在途请求体字节预算 |
| `admin.header` | `Authorization` | 运维接口鉴权头 |
| `admin.token` | 未配置 | 运维 Bearer token；为空时运维路由不会注册 |
| `log.level` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `store.path` | `data/webhook.db` | SQLite 文件路径 |
| `store.retention` | `720h` | 终态事件与动作日志的保留时间（按 `processed_at`，缺省回退 `received_at`）；`pending`/`processing`/`retrying` 不会被清理 |
| `store.retain_payload` | `false` | `false` 时，命中规则的事件在 `vars_json`/action 状态同事务提交后清除 payload；skipped 在终态提交后清除；预处理失败的 dead 事件保留 payload 供排障/replay |
| `queue.workers` | `4` | 异步处理 worker 数量（1–64） |
| `queue.buffer` | `256` | 内存唤醒信号容量，不是持久任务容量 |
| `queue.max_retries` | `2` | 首次执行后的额外重试次数（0–20）；0 表示只尝试一次 |
| `queue.retry_base` | `1s` | 指数退避基础时间 |
| `queue.max_retry_delay` | `15m` | 抖动退避上限；服务端 `Retry-After` 不会被静默截短 |
| `queue.max_retry_age` | `24h` | 一个事件允许自动重试的最长期限，超过后进入 `dead` |
| `heartbeat.interval` | `60s` | 死链心跳周期（10s–1h），也是对外监控的告警时间基准 |
| `heartbeat.timeout` | `10s` | 每个心跳 target 的 HTTP 超时（1s–30s，且不超过 interval） |
| `heartbeat.host` | `general-webhook` | 心跳 JSON 负载里的主机标识；不固定，多机部署应每台设置唯一 `HOST_LABEL`，未设置时回退该默认值 |
| `heartbeat.targets` | 空 | 外部监控端点列表；URL 全部为空时模块不启动（fail closed） |

### Source 配置

| 字段 | 说明 |
| --- | --- |
| `name` | 事件源名称，对应 `/webhook/{name}` |
| `auth` | 鉴权配置，必须显式声明 `type` |
| `extract` | 变量名到 gjson 路径的映射，支持 `$.a.b` 写法 |
| `rules` | 正则过滤规则，所有规则都命中才执行动作；空数组表示总是触发 |
| `actions` | 动作列表，按顺序执行 |

`hmac` 和 `token` 类型必须配置至少 16 字节的 `secret`。内部 relay 若设置 `signed_headers`，必须精确使用 `[timestamp, delivery_id, body]`；空数组保留 GitHub 兼容的仅 body 签名。
示例配置默认包含 `github`、`custom`、`bt`、`dingtalk-demo` 四个 source；使用默认配置启动时，请提供其中 token/HMAC source 需要的 secret 环境变量，或删除暂不启用的 source。

**环境变量处理**：

- `auth.secret` / `admin.token` 的 `${VAR}` 在配置加载时展开，但不会进入动作快照。
- `${event:name}` 只读取事件变量；`${secret:NAME}` 在启动校验时必须存在、在 action 执行时读取环境变量，快照保存引用而非凭据。
- 旧式 `${name}` 为兼容语法：先读取事件变量，未命中时读取同名环境变量；新增配置应优先使用显式命名空间。

示例：

```yaml
sources:
  - name: github
    auth:
      type: hmac
      secret: ${GITHUB_SECRET}  # 配置加载时从环境变量注入
    extract:
      repo: $.repository.name   # 从 payload 抽取
    actions:
      - type: http
        url: ${secret:FEISHU_BOT} # 执行时读取环境变量；快照只保存引用
        body: |                 # body 中的 ${repo} 是运行时变量，从 extract 获取
          {"text": "${repo} updated"}
```

### 鉴权方式

| 类型 | 请求格式 |
| --- | --- |
| `hmac` | 默认读取 `X-Hub-Signature-256: sha256=<hex>`，算法为 HMAC-SHA256 |
| `token` | 仅读取专用请求头（默认 `X-Webhook-Token`）；不再支持查询参数，避免进入访问日志 |
| `none` | 不校验来源，仍需显式配置 `auth.type: none` |

`hmac` 与 `token` 校验都使用常量时间比较。

### 动作类型

本网关只提供 **`http`** 和 **`exec`** 两种通用动作，不针对具体 SaaS 或 IM 平台做硬编码适配。任何 HTTP webhook API 都可通过 `http` + 自定义 body 模板接入。

#### http

通用 HTTP 请求，支持完全自定义 URL/Method/Headers/Body。事件值使用 `${event:name}`（兼容 `${name}`），凭据使用 `${secret:NAME}`。

**Telegram bot 示例**：

```yaml
- type: http
  url: "https://api.telegram.org/bot${secret:TG_BOT_TOKEN}/sendMessage"
  method: POST
  headers:
    Content-Type: application/json
  body: |
    {
      "chat_id": "${secret:TG_CHAT_ID}",
      "text": "告警: ${title}\n${msg}"
    }
```

**飞书群机器人示例**：

```yaml
- type: http
  url: ${secret:FEISHU_BOT}
  method: POST
  headers:
    Content-Type: application/json
  body: |
    {
      "msg_type": "text",
      "content": {"text": "${repo} pushed to ${branch}"}
    }
```

**钉钉群机器人示例**：

```yaml
- type: http
  url: "https://oapi.dingtalk.com/robot/send?access_token=${secret:DINGTALK_TOKEN}"
  method: POST
  body: |
    {
      "msgtype": "text",
      "text": {"content": "${message}"}
    }
```

**转发到自定义服务（如 n8n）**：

```yaml
- type: http
  url: "https://n8n.local/webhook/deploy"
  method: POST
  allow_private: true
  url_allowlist: ["n8n.local"]
  headers:
    X-Webhook-Source: general-webhook
  body: |
    {
      "event": "${event}",
      "branch": "${branch}",
      "repo": "${repo}"
    }
```

字段说明：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `url` | ✅ | 目标 URL，支持 `${var}` 插值 |
| `method` | ❌ | HTTP 方法，默认 `POST` |
| `headers` | ❌ | 自定义请求头，key/value 都支持插值 |
| `body` | ❌ | 请求体模板，支持 `${var}` 插值；JSON 字符串值会做转义；为空时不设 body |
| `url_allowlist` | ❌ | 结构化 host、host:port、CIDR 或 URL 路径白名单；未配置时仅允许公网 `https` |
| `allow_private` | ❌ | 默认 `false`；私网 DNS/IP 目标必须同时显式启用并命中 allowlist |

默认 `Content-Type: application/json`（`body` 非空且未显式设置时），可通过 `headers` 覆盖。每次 HTTP action 尝试的总超时固定为 15 秒。初始 URL、DNS 解析结果及每次重定向都会校验；跨主机重定向、loopback、link-local、metadata 与 DNS rebinding 会被拒绝。出站请求不继承 `HTTP_PROXY`，如需代理应把经过评审的网关配置为明确目标。

**出站目标代理**：若容器访问 Telegram 等目标必须经过代理或兼容网关，可把 action 的 `url` 改为经过评审的出站地址，并把它加入 `url_allowlist`。它只改变 Webhook 发出的请求，不表示可以把本服务通过反向代理暴露到公网。

#### exec

执行本地命令或脚本。

```yaml
- type: exec
  command: "/opt/general-webhook/scripts/run.sh"
  args: ["${job}"]
  timeout: 300s
```

`exec` 不经过 shell，`command` 与 `args` 会逐个传入 `exec.Command`；超时通过独立 context 与进程组终止实现。默认超时为 `60s`，输出最多保存 8 KB。

### 变量插值

动作配置里的字符串支持 `${event:var}`、`${secret:NAME}` 和兼容形式 `${var}`，用于：

- `http.url`
- `http.headers` 的 key 和 value
- `http.body`
- `exec.args`

变量优先来自当前 source 的 `extract` 配置；未命中时读取同名环境变量；仍不存在时为空字符串。HTTP JSON body 中的变量会按 JSON 字符串规则转义，避免消息里的引号、反斜杠或换行打坏 JSON。

## 心跳模块（Deadman Heartbeat）

webhook 是告警转发中枢，但标准部署不发布宿主端口，外部探针无法入站访问 `/healthz` 或 `/readyz`——"谁来监控监控者"是个真实缺口。心跳模块让进程**自己向外部监控端点主动外推存活信号**（与 Runtime 项目的 `runtime-heartbeat.sh` → Healthchecks/Uptime Kuma/Cloudflare Pages 死链模式一致，但内置在 Go 进程内，不依赖 cron 或脚本）：

```text
进程内 goroutine (周期 interval)
  -> 评估健康: queue workers 存活 && server 未关停 && store 可读
  -> 通过: POST url (push 模式) 或 POST 结构化 JSON (json 模式)
  -> 不健康: POST url+/fail (push 模式) 或 status:"fail" (json 模式)
  -> 目标返回 4xx/5xx 或网络错误: 记错误日志, 不计成功
```

- **`push` 模式（默认）**：健康时 POST 到 `url`，不健康时 POST 到 `url + "/fail"`——Healthchecks.io 的标准语义；Uptime Kuma push 对任何请求都记作一次心跳，超时未收到才告警。
- **`json` 模式**：始终 POST 到 `url`，负载携带 `host` / `status`（`pass`/`fail`，与 Runtime `cloudflare-heartbeat` 接收器一致）/ `exit_code` / `timestamp` / `uptime_seconds` / `revision`（`GENERAL_WEBHOOK_GIT_COMMIT`）/ `stats`（队列计数、5m/1h 动作失败率、最老未完成事件年龄）/ `drop_summary`（log_policy 拒绝摘要）。
- `host`（`heartbeat.host`，默认经 `${HOST_LABEL}` 注入）**不是固定值**：多机部署时应为每台主机设置唯一的 `HOST_LABEL` 以便区分；未设置时负载回退为固定默认 `general-webhook`。
- 每个 target 可带 `token`，非空时作为 `X-Heartbeat-Token` 头发送。
- 日志与错误只记录 `scheme://host[:port]`，**绝不记录 path/query**——Healthchecks/Uptime Kuma 的 push URL 自带一次性 token，绝不能进日志。
- 出站客户端：`Proxy=nil`（不继承 `HTTP_PROXY`）、**不跟随重定向**（防止跨主机重定向把 token 头带走）、固定超时、URL 默认必须 `https`（内部 Uptime Kuma 等需显式 `allow_http: true`）、禁止 URL 内嵌 userinfo。
- 优雅关停时**不发 fail 信号**：死链监控的 grace 吸收重启窗口，否则每次部署都会误报。首个心跳在 `interval` 之后发出，因此 grace 应大于 `interval` + 预期重启时间。
- 命中规则：监控应覆盖两类信号——**进程消失**（心跳停止 → 死链告警）与**进程在但不健康**（心跳持续但 status=fail / `url+/fail`），后者可再叠加 Runtime 的 `HEARTBEAT_WEBHOOK_URL` 通道把失败细节送进 Telegram。

示例：在 `.env` 配置（URL 未设置时模块自动关闭，无需改镜像）：

```bash
HEARTBEAT_URL=https://hc-ping.com/<check-uuid>/webhook
HEARTBEAT_TOKEN=
CF_HEARTBEAT_URL=https://runtime-heartbeat.pages.dev/api/heartbeat
CF_HEARTBEAT_TOKEN=<与 CF Pages 相同的 token>
HOST_LABEL=tokyo   # 可选: 每台主机设唯一值; 未设置时心跳 host 回退为 general-webhook
```

```bash
# 手动验证（Healthchecks 风格）
curl -fsS -X POST "$HEARTBEAT_URL"
curl -fsS -X POST "$HEARTBEAT_URL/fail"
```

## 审计与日志

SQLite 会自动创建持久状态与审计表：

| 表 | 内容 |
| --- | --- |
| `events` | source、状态、`vars_json`、毫秒时间、replay 代次和动作快照；按要求使用 `${secret:NAME}` 时只保存引用 |
| `event_actions` | 每个 action 的状态、尝试次数、`next_attempt_at`、最近错误与完成时间 |
| `action_logs` | 每次尝试的 action index、replay 代次、已脱敏 target/detail 和耗时 |
| `replay_guard` | HMAC body 的原子、带过期时间重放保护键 |

日志输出到 stdout，格式为 JSON。标准 Compose 使用有界的 Docker `json-file` 日志（10 MiB × 5）；若另接 Loki/ELK，应保留等价的容量或保留期边界。日志以及写入 SQLite 的 action target/detail 都会先脱敏；HTTP target 只保留 `scheme://host[:port]`，不保存可能携带凭据的 path/query。数据目录权限为 `0700`、数据库文件为 `0600`；`store.retention` 默认保留终态事件 30 天。

HMAC 重放边界：GitHub 兼容模式签名仅覆盖 body，但必须同时提供 delivery ID 与 `X-Webhook-Timestamp`；同 source/body 的 replay guard 与事件写入在同一事务完成。内部 relay 可配置 `auth.signed_headers: [timestamp, delivery_id, body]` 做密码学绑定。动作凭据必须写成 `${secret:NAME}`，此时快照只保存引用；不要在 URL、header、body 或 args 中内联凭据。HTTP action 的 `Idempotency-Key` 固定为 `event_id:action_index`；`4xx`（除 408/429）不重试，并持久化 `Retry-After`。

典型日志字段：

| 字段 | 说明 |
| --- | --- |
| `trace_id` | 等于 `event_id`，用于串联一次请求的全链路日志 |
| `source` | webhook 来源 |
| `type` | 动作类型 |
| `target` | 动作目标；HTTP 仅记录 origin，exec 记录 command |
| `attempt` | 第几次尝试 |
| `duration_ms` | 动作耗时 |

## 安全边界

- 唯一推荐部署：Dockerfile 构建 + Docker Compose；见 [INSTALL.md](INSTALL.md) 与 [deploy/README.md](deploy/README.md)。
- `auth.type` 必须显式配置，避免漏配时意外放行。
- `hmac` / `token` 的 `secret` 至少 16 字节，配置加载时会校验。
- 标准 Compose 不发布宿主机端口；入口仅存在于受控的 `webhook-internal` 网络。
- 容器使用固定非 root 用户、只读根文件系统、`cap_drop: ALL` 和 `no-new-privileges`。
- CPU、内存与 swap、PID、`nofile`、临时目录和 Docker JSON 日志都有默认边界。
- `.env` 必须为 `0600`，数据目录必须为 `0700` 且归容器 UID/GID 所有。
- 请求体最大 1 MiB；进程级并发与在途字节预算限制鉴权前内存峰值。
- SQLite outbox 是任务事实来源；内存通道只发送唤醒信号，进程重启后仍会继续认领未完成任务。
- 关停取消的 in-flight 事件回退为 `pending`，不会写成终态 `partial`。
- `exec` 不经过 shell，减少命令注入风险。
- 外部命令有超时限制，输出会截断后再进入审计记录。
- HTTP 动作未配置 `url_allowlist` 时仅允许公网 HTTPS；配置后目标必须命中，私网目标还必须显式设置 `allow_private: true`。
- 运维接口只有配置 `admin.token` 后才注册；默认 `Authorization` header 要求 Bearer token，并有独立限流。
- 使用 `${secret:NAME}` 的动作凭据在快照中只保存引用；启动时会清理旧快照中仍可识别的当前凭据值。
- 日志以及写入审计库的 action target/detail 会在持久化前脱敏。
- SQLite 使用单写连接，避免并发写导致 `database is locked`。

## 运维接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/events/{id}` | 事件状态与近期 action_logs |
| `GET` | `/queue/stats` | 队列状态计数与最老未完成事件年龄 |
| `POST` | `/events/{id}/retry` | 只重置 `dead` action；成功 action 不会重放 |
| `GET` | `/metrics` | Prometheus 文本指标 |

这些接口与 webhook 共用监听地址，同时受 Docker 网络和管理 token 两层保护。默认 `admin.header: Authorization` 时必须携带 `Authorization: Bearer $ADMIN_TOKEN`；若改为自定义 header，则直接传原始 token，不加 `Bearer`。未配置 token 时路由返回 404。replay 可选提交不超过 256 字节的审计原因：

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://general-webhook:8080/queue/stats
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"reason":"downstream recovered"}' http://general-webhook:8080/events/EVENT_ID/retry
```

旧版本 `partial` / `error` 事件没有 `event_actions` 明细时，人工 replay 会初始化并重放该事件的全部 action；操作前必须确认下游幂等性。新版本 `dead` 事件只重置失败 action，已经 `done` 的 action 保持不变；`max_retry_age` 从本次 replay 时间重新计算。

`/metrics` 暴露事件/动作队列深度、最老未完成事件年龄，以及 5 分钟和 1 小时滚动 action 尝试数与失败率。至少应对以下条件告警：

- `general_webhook_events{status=~"dead|partial|error"} > 0`
- `general_webhook_oldest_pending_age_milliseconds` 超过业务允许的未完成时限
- `general_webhook_action_failure_ratio{window="5m"}` 持续升高（同时用 `general_webhook_action_attempts` 设置最小样本量）
- `general_webhook_heartbeat_last_success_timestamp` 与当前时间差超过 `heartbeat.interval` 的若干倍（进程内观测；进程整体消失则由死链监控负责）

## 当前限制

- SQLite outbox 是单节点持久队列，不是分布式队列；单实例重启会重置本进程遗留的 `processing` 租约。
- `retain_payload=false`（默认）时，命中规则的事件在 `vars_json` 和 action 状态提交成功后清除 payload，skipped 事件在提交终态后清除；预处理失败的 dead 事件通常保留 payload。已持久化变量的 dead replay 不依赖 payload；旧版本已清空且没有 `vars_json` 的事件会明确返回 `409`。
- 规则正则在每次事件处理时编译，当前更偏向配置简单和实现直接。
- `exec.command` 由镜像内配置控制，目前没有额外的允许目录白名单。
- `restart: unless-stopped` 只在进程退出时重启；单纯 `unhealthy` 不会触发 Docker 自动重启。默认由心跳模块（外部死链监控）负责告警：进程消失 → 心跳停止告警，进程在但不健康 → `status=fail`；如需自动重启，应在宿主机配合 Uptime Kuma/Docker autoheal 等外部机制。
- 心跳依赖外部监控端点可达；若宿主机整机断网或断电，心跳与告警通道可能同时失效，仍应由独立的远端探针/基础设施监控兜底（与 Runtime 的 `NETWORK_TOPOLOGY.md` 结论一致）。

## 后续方向

- 对规则正则做启动期预编译。
- 增加 `exec` 命令白名单或允许目录校验。
- 审计库增加定时备份，例如 SQLite 快照到 S3 兼容存储。
- 规模变大时，将 SQLite durable outbox 迁移到支持多实例认领的数据库或消息系统，并把 worker 独立扩展。

## 设计理念

**为什么只有 http 和 exec 两种 action？**

早期版本有 `notify`（白名单 feishu/dingtalk/wecom）和 `n8n` 两种专用 action，但每次加新渠道（telegram/discord/bark）都要改 Go 代码编译部署。

重构后，所有 HTTP webhook 统一用 `http` type + 自定义 body 模板适配：

- Telegram/飞书/钉钉/Discord/Slack/自建服务：**纯配置，零代码**
- body 模板支持 `${var}` 插值，灵活度等同硬编码
- 本地开发时配置变更后重启进程即可；标准 Docker 部署中配置和脚本已烘焙进镜像，修改后必须重新构建带版本标签的镜像并重新部署

这符合"通用 webhook 网关"定位：提供稳定的接入层和异步队列，不与具体 SaaS 绑定。
