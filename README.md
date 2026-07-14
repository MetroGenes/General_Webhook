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
- 审计记录：SQLite 保存事件元数据、处理状态和已脱敏的动作日志；原始 payload 是否长期保留由 `store.retain_payload` 控制。
- 结构化日志：使用标准库 `log/slog` 输出 JSON 日志，`event_id` 同时作为 `trace_id`。

## 工作流程

```text
POST /webhook/github
  -> 校验 source 是否存在
  -> 读取并限制 body 大小，最大 5 MB
  -> 按 source.auth 校验签名或 token
  -> 生成 event_id / trace_id
  -> 以 pending 状态持久化到 SQLite outbox
  -> 唤醒 worker（内存通道只作为唤醒信号）
  -> 返回 202 Accepted

worker 异步处理
  -> 按 extract 抽取变量
  -> 按 rules 过滤事件
  -> 依次执行 actions
  -> 失败动作按 max_retries 线性退避重试
  -> 更新事件状态并写入 action_logs
```

事件状态：

| 状态 | 含义 |
| --- | --- |
| `received` | 旧版本兼容状态；启动迁移时转为 `pending` |
| `pending` | 已持久化，等待 worker 认领 |
| `processing` | worker 已认领，执行中 |
| `done` | 所有动作执行成功 |
| `partial` | 至少一个动作在重试后仍失败 |
| `skipped` | 规则未命中，未执行动作 |
| `error` | 规则匹配等系统处理过程出错 |

## 项目结构

```text
.
├── cmd/server/main.go              # 程序入口、HTTP server、优雅关停
├── internal/
│   ├── auth/                       # HMAC / token 鉴权
│   ├── config/                     # YAML 配置、环境变量注入、校验
│   ├── handler/                    # http / exec 动作处理器
│   ├── logger/                     # slog JSON 日志与 trace_id
│   ├── parser/                     # gjson 变量抽取
│   ├── queue/                      # SQLite outbox worker、唤醒、租约与重试
│   ├── router/                     # 正则规则匹配
│   ├── server/                     # HTTP 路由与 webhook 接入层
│   └── store/                      # SQLite outbox、审计、保留与 payload 清理
├── configs/webhooks.yaml           # 示例配置
├── deploy/                         # Docker 部署同步助手；systemd 文件仅为历史兼容
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

项目要求 Go `1.26.5`。先删除示例配置中不使用的 source，或为其提供所需环境变量。开发进程也必须只监听 loopback；复制配置并修改监听地址后再运行：

```bash
export GITHUB_SECRET='your_github_secret'
export FEISHU_BOT='https://open.feishu.cn/open-apis/bot/v2/hook/xxx'
export CUSTOM_TOKEN='your_token'
export BT_TOKEN='baota_token'
export TG_BOT_TOKEN='123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11'
export TG_CHAT_ID='123456789'
export DINGTALK_TOKEN='dingtalk_access_token'
export DINGTALK_WEBHOOK_TOKEN='dingtalk_inbound_token'

cp configs/webhooks.yaml /tmp/general-webhook-dev.yaml
sed -i '0,/addr: ":8080"/s//addr: "127.0.0.1:8080"/' /tmp/general-webhook-dev.yaml
go run ./cmd/server -config /tmp/general-webhook-dev.yaml
```

该方式不具备 Compose 的网络与资源边界，只用于隔离的开发环境；不要直接用监听 `:8080` 的原始示例配置启动开发服务。

```bash
curl http://127.0.0.1:8080/readyz
curl -X POST 'http://127.0.0.1:8080/webhook/custom' \
  -H 'Content-Type: application/json' \
  -H 'X-Webhook-Token: your_token' \
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

log:
  level: "info"

store:
  path: "data/webhook.db"
  retention: 720h
  retain_payload: true

queue:
  workers: 4
  buffer: 256
  max_retries: 3

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
        regex: "refs/heads/(main|master)"
    actions:
      - type: http
        url: ${FEISHU_BOT}
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
        url: "https://api.telegram.org/bot${TG_BOT_TOKEN}/sendMessage"
        method: POST
        headers:
          Content-Type: application/json
        body: |
          {
            "chat_id": "${TG_CHAT_ID}",
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
| `log.level` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `store.path` | `data/webhook.db` | SQLite 文件路径 |
| `store.retention` | `720h` | 终态事件与动作日志的保留时间；`pending`/`processing` 不会被清理 |
| `store.retain_payload` | `true` | 是否在处理完成后继续保留原始 payload；设为 `false` 可缩小敏感数据留存 |
| `queue.workers` | `4` | 异步处理 worker 数量 |
| `queue.buffer` | `256` | 内存唤醒信号容量，不是持久任务容量 |
| `queue.max_retries` | `3` | 每个动作的最大尝试次数 |

### Source 配置

| 字段 | 说明 |
| --- | --- |
| `name` | 事件源名称，对应 `/webhook/{name}` |
| `auth` | 鉴权配置，必须显式声明 `type` |
| `extract` | 变量名到 gjson 路径的映射，支持 `$.a.b` 写法 |
| `rules` | 正则过滤规则，所有规则都命中才执行动作；空数组表示总是触发 |
| `actions` | 动作列表，按顺序执行 |

`hmac` 和 `token` 类型必须配置非空 `secret`。
示例配置默认包含 `github`、`custom`、`bt`、`dingtalk-demo` 四个 source；使用默认配置启动时，请提供其中 token/HMAC source 需要的 secret 环境变量，或删除暂不启用的 source。

**环境变量处理**：

- **配置层字段**（`auth.secret`、`action.url`、`action.command`）支持 `${VAR}` 环境变量注入，在配置加载时展开
- **运行时插值字段**（`action.body`、`action.headers`、`action.args`）中的 `${var}` 先从 `extract` 抽取的变量中替换，未命中时再读取同名环境变量

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
        url: ${FEISHU_BOT}      # 配置加载时从环境变量注入
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

通用 HTTP 请求，支持完全自定义 URL/Method/Headers/Body，所有字段支持 `${var}` 插值。

**Telegram bot 示例**：

```yaml
- type: http
  url: "https://api.telegram.org/bot${TG_BOT_TOKEN}/sendMessage"
  method: POST
  headers:
    Content-Type: application/json
  body: |
    {
      "chat_id": "${TG_CHAT_ID}",
      "text": "告警: ${title}\n${msg}"
    }
```

**飞书群机器人示例**：

```yaml
- type: http
  url: ${FEISHU_BOT}
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
  url: "https://oapi.dingtalk.com/robot/send?access_token=${DINGTALK_TOKEN}"
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

默认 `Content-Type: application/json`（`body` 非空且未显式设置时），可通过 `headers` 覆盖。

**出站目标代理**：若容器访问 Telegram 等目标必须经过代理或兼容网关，可把 action 的 `url` 改为经过评审的出站地址。它只改变 Webhook 发出的请求，不表示可以把本服务通过反向代理暴露到公网；使用第三方代理时还应评估 URL token 和消息内容的泄露风险。

#### exec

执行本地命令或脚本。

```yaml
- type: exec
  command: "/opt/general-webhook/scripts/run.sh"
  args: ["${job}"]
  timeout: 300s
```

`exec` 不经过 shell，`command` 与 `args` 会逐个传入 `exec.CommandContext`。默认超时为 `60s`，输出最多保存 8 KB。

### 变量插值

动作配置里的字符串支持 `${var}` 插值，用于：

- `http.url`
- `http.headers` 的 key 和 value
- `http.body`
- `exec.args`

变量优先来自当前 source 的 `extract` 配置；未命中时读取同名环境变量；仍不存在时为空字符串。HTTP JSON body 中的变量会按 JSON 字符串规则转义，避免消息里的引号、反斜杠或换行打坏 JSON。

## 审计与日志

SQLite 会自动创建两个表：

| 表 | 内容 |
| --- | --- |
| `events` | source、remote_ip、状态与时间；payload 在 `retain_payload=false` 时会于处理完成后清除 |
| `action_logs` | 每次动作尝试的 action、已脱敏 target/detail、attempt、success、duration_ms |

日志输出到 stdout，格式为 JSON。标准 Compose 使用有界的 Docker `json-file` 日志（10 MiB × 5）；若另接 Loki/ELK，应保留等价的容量或保留期边界。日志以及写入 SQLite 的 action target/detail 都会先对常见 URL token 脱敏。数据目录权限为 `0700`、数据库文件为 `0600`；`store.retention` 默认保留终态事件 30 天。

HMAC 重放边界：签名仅覆盖 body（供应方协议）。运营层去重键为供应方 delivery ID（及可选时间窗），不构成密码学绑定；相同 body 只要 delivery ID 不同即可被接受。HTTP action 的 `Idempotency-Key` 在整个重试过程中固定为 `event_id`，attempt 通过 `X-Webhook-Attempt` 传递；exec 动作须由脚本自身保证幂等。

典型日志字段：

| 字段 | 说明 |
| --- | --- |
| `trace_id` | 等于 `event_id`，用于串联一次请求的全链路日志 |
| `source` | webhook 来源 |
| `type` | 动作类型 |
| `target` | 动作目标，如 channel、URL 或 command |
| `attempt` | 第几次尝试 |
| `duration_ms` | 动作耗时 |

## 历史兼容部署

`deploy/general-webhook.service` 与 `deploy/general-webhook.env.example` 仅保留给已有 systemd 安装做迁移参考，不属于受支持的标准生产部署，也不保证与 Docker 的镜像内容、资源和网络边界等价。新部署及迁移目标都应使用 Dockerfile 与 `docker-compose.yaml`；不要同时运行 Docker 和 systemd 实例并共享同一个 SQLite 数据目录。

## 安全边界

- `auth.type` 必须显式配置，避免漏配时意外放行。
- `hmac` / `token` 的 `secret` 不能为空，配置加载时会校验。
- 标准 Compose 不发布宿主机端口；入口仅存在于受控的 `webhook-internal` 网络。
- 容器使用固定非 root 用户、只读根文件系统、`cap_drop: ALL` 和 `no-new-privileges`。
- CPU、内存与 swap、PID、`nofile`、临时目录和 Docker JSON 日志都有默认边界。
- `.env` 必须为 `0600`，数据目录必须为 `0700` 且归容器 UID/GID 所有。
- 请求体最大 5 MB，超过直接拒绝。
- SQLite outbox 是任务事实来源；内存通道只发送唤醒信号，进程重启后仍会继续认领未完成任务。
- `exec` 不经过 shell，减少命令注入风险。
- 外部命令有超时限制，输出会截断后再进入审计记录。
- 日志以及写入审计库的 action target/detail 会在持久化前脱敏。
- SQLite 使用单写连接，避免并发写导致 `database is locked`。

## 当前限制

- SQLite outbox 是单节点持久队列，不是分布式队列；`pending` 会在重启后继续处理，租约过期的 `processing` 会被重新认领。
- 当前没有 HTTP replay 端点；`retain_payload=false` 时，终态事件也没有 payload 可供重放。
- `webhook-egress` 提供必要的出站连接，但 Docker bridge 本身不是目标域名白名单；需要严格限制目的地时，应在宿主机防火墙或专用出站代理实施策略。
- 规则正则在每次事件处理时编译，当前更偏向配置简单和实现直接。
- `exec.command` 由镜像内配置控制，目前没有额外的允许目录白名单。
- `restart: unless-stopped` 只在进程退出时重启；单纯 `unhealthy` 不会触发 Docker 自动重启，应由监控系统告警。

## 后续方向

- 增加 `POST /replay/{event_id}` 事件重放端点。
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
