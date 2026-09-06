# General Webhook

General Webhook 是一个用 Go 编写的通用 webhook 网关。它提供统一的 HTTP 入口，负责接收事件、校验来源、从 JSON payload 中抽取变量、按规则过滤事件，并异步执行配置好的动作。

项目的标准部署面向 Docker 内部的监控与应用事件，例如 Fluent Bit、容器状态监测器和同机业务容器。生产环境默认拉取 GHCR 镜像、使用 Docker Compose 运行；定制配置和脚本时再通过 Dockerfile 构建自己的镜像。默认不发布宿主机端口，也不接入公网反向代理。GitHub、宝塔等公网来源若确需接入，应先经过独立的内部 relay/agent。

## 功能概览

- 容器内统一入口：`POST /webhook/{source}`，`source` 对应配置里的事件源名称。
- 来源鉴权：支持 HMAC-SHA256、静态 token，也可以显式配置为 `none`；GitHub relay 示例默认绑定时间戳、delivery ID 和 body。
- 变量抽取：使用 `gjson` 从 JSON payload 抽取变量，配置里可写 `$.a.b`。
- 规则过滤：对抽取变量做正则匹配，所有规则命中才执行动作；可选出口 allowlist 与消息脱敏。
- 异步处理：请求写入 SQLite outbox（`pending`）后返回 `202 Accepted`，worker 认领并执行动作；进程重启可继续处理未完成任务。
- 动作类型：通用 HTTP 请求（支持 Telegram/飞书/钉钉/自定义服务）+ 本地命令执行。
- 审计记录：SQLite 保存事件、每个 action 的持久重试状态、耐久化抽取变量、已脱敏动作日志和 heartbeat push 错误；原始 payload 是否长期保留由 `store.retain_payload` 控制。
- 结构化日志：使用标准库 `log/slog` 输出 JSON 日志，`event_id` 同时作为 `trace_id`。
- 死链心跳：默认主动向 Uptime Kuma Push 监控项报告本地就绪状态，支持 Healthchecks 和 JSON 接收器；每个目标独立调度，不配置 target 时模块不启动。

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
  -> 按 log_policy allowlist 与 rules 过滤，拒绝事件提交 skipped
  -> 对待投递变量脱敏，执行元数据独立保存
  -> 命中后，同一事务持久化 vars_json 并初始化 action 状态
  -> 逐个执行尚未成功的 actions
  -> 失败动作写入持久 next_attempt_at，按指数退避 + full jitter 重试
  -> Retry-After 到期前不占用 worker；实际派发前再次检查期限
  -> 尚未首试或待重试动作到达 max_retry_age 后进入 dead letter
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
| `skipped` | 规则未命中或出口 allowlist 拒绝，未执行动作 |

旧数据库中的 `partial` / `error` 只作为迁移兼容状态保留。

## 项目结构

```text
.
├── cmd/server/main.go              # 程序入口、HTTP server、优雅关停
├── internal/
│   ├── auth/                       # HMAC / token 鉴权
│   ├── config/                     # YAML 配置、环境变量注入、校验
│   ├── handler/                    # http / exec 动作处理器
│   ├── heartbeat/                  # Uptime Kuma / Healthchecks / JSON 主动心跳
│   ├── logger/                     # slog JSON 日志与 trace_id
│   ├── parser/                     # gjson 变量抽取
│   ├── policy/                     # 出口 allowlist、脱敏、有界拒绝统计
│   ├── queue/                      # SQLite outbox worker、唤醒、租约与重试
│   ├── router/                     # 正则规则匹配与有界编译缓存
│   ├── server/                     # HTTP 路由与 webhook 接入层
│   └── store/                      # SQLite outbox、审计、保留与 payload 清理
├── configs/webhooks.yaml           # 示例配置
├── deploy/                         # 部署说明（指向 Dockerfile + Compose）
├── scripts/run.sh                  # exec 动作示例脚本
├── scripts/test-kuma.sh            # 真实 Uptime Kuma 容器集成测试
├── INSTALL.md                      # Docker Compose 正式部署指南
├── CHANGELOG.md                    # 本次审计修复与升级说明
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

### 标准生产部署（GHCR + Compose）

完整步骤和运维说明见 [INSTALL.md](INSTALL.md)。`.env.example` 默认选择 `ghcr.io/metrogenes/general_webhook:1.0.0`，支持 `linux/amd64` 与 `linux/arm64`。标准部署只挂载 SQLite 数据目录；`configs/` 与 `scripts/` 已烘焙进镜像。以下命令使用 `/data/webhook` 与 `webhook-internal`，覆盖路径或网络名时应同步替换相关命令和生产者配置。

```bash
cp .env.example .env
chmod 600 .env
# 编辑 .env：填写默认配置中已启用 source/action 所需的 secret 与管理 token

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
docker compose --env-file .env pull webhook
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
```

`1.0.0` 是按本项目部署约定持续更新的镜像标签，与 `latest` 一起由主分支 CI 更新。需要固定版本或回滚时，保存 `ghcr.io/metrogenes/general_webhook:sha-<完整提交 SHA>` 或镜像 digest。旧 Git `v1.0.0` 标签保留原提交，不用于移动部署标签。

如需删减示例 source、修改脚本或为内部 HTTP Kuma 开启 `allow_http`，先把 `.env` 的 `WEBHOOK_IMAGE` 改为自己的唯一标签，例如 `general-webhook:local-20260906-01`，然后执行 `docker compose --env-file .env build --pull` 和上述 `up --no-build`。每次定制都使用新标签；不要把定制内容构建到官方部署标签名下。

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

使用最新稳定版 Go；`go.mod` 中的 `go 1.26.6` 是最低版本要求，不锁定 CI 或镜像构建工具链。本次本机验证使用 Go `1.27.1`。先删除示例配置中不使用的 source，或为其提供所需环境变量。开发进程只监听 loopback；复制配置并修改监听地址后再运行：

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

### 构建、测试与发布

CI 使用 `actions/setup-go` 的 `go-version: stable` 与 `check-latest: true`；Docker builder 使用 `golang:alpine`，构建时启用 `--pull` / `pull: true`。每次构建解析当时的最新稳定 Go，具体版本记录在构建日志中。

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/general-webhook ./cmd/server
REVIEW_BINARY=/tmp/general-webhook go test -tags=process_integration -count=1 -timeout=65s ./cmd/server
bash scripts/test-kuma.sh
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck -version
govulncheck ./...
```

本次常规单元测试、race、vet 和二进制构建已通过，语句覆盖率为 **71.4%**。`process_integration` 验证真实进程强制退出/恢复、35 秒关停预算和关停时停止心跳，本次通过耗时 45.096s，drain 为 35.028s。使用当前 Go 编译的 govulncheck v1.7.0 源码扫描未发现已知漏洞；CI 安装最新版并记录版本，避免旧工具无法解析新工具链产物。

Kuma 集成测试需要本机 Docker 和 Go，使用临时 Kuma 容器、随机测试凭据与临时 SQLite 数据，结束后自动清理容器和临时文件；Node.js 来自容器，无需宿主机安装。本次已与 Uptime Kuma `2.5.3` 联调。该脚本也纳入 CI。

发布 job 必须等待同一提交的格式检查、vet、测试、覆盖率门槛、竞态检测、配置/Compose 校验、真实进程测试、Docker 构建、Kuma 集成测试和 govulncheck 全部成功。Actions 固定提交 SHA，发布附带 provenance 与 SBOM。主分支发布更新 `1.0.0`、`latest` 和 `sha-<完整提交 SHA>`，同时构建 amd64/arm64；具体发布结果以远端 CI 为准。按提交标签可定位代码版本，若要固定完全相同的镜像产物，应使用 digest。

官方镜像通过构建参数 `VCS_REF` 内置 revision。自建镜像可在 `.env` 设置 `GENERAL_WEBHOOK_GIT_COMMIT`，Compose 仅把它传给 `build.args.VCS_REF`，不在运行时覆盖官方镜像的 revision。审计修复与升级注意事项见 [CHANGELOG.md](CHANGELOG.md)。

## 配置说明

完整示例见 `configs/webhooks.yaml`。新增接入时，通常只需要在 `sources` 下增加一个 source。

**核心理念**：action 只有 `http` 和 `exec` 两种类型，`http` 支持完全自定义 URL/Headers/Body，通过模板插值适配任意 webhook API（Telegram、飞书、钉钉、Discord、Slack、自建服务等），零代码扩展。

```yaml
server:
  addr: ":8080"
  trusted_proxies: []
  rate_limit:
    pre_auth_per_ip: {burst: 120, per_second: 20}
    global: {burst: 120, per_second: 2}
    source_per_ip: {burst: 30, per_second: 0.5}
    admin_per_ip: {burst: 60, per_second: 1}

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
    - name: kuma
      url: ${HEARTBEAT_URL}     # Kuma /api/push/<token>?status=up&msg=OK&ping=
      token: ${HEARTBEAT_TOKEN} # 可选, 作为 X-Heartbeat-Token 头发送
      mode: kuma               # 默认；另支持 healthchecks / push(旧别名) / json
      allow_http: false        # 内部 HTTP Kuma 需显式开启，并重建自有镜像
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
      signed_headers: [timestamp, delivery_id, body]
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
| `server.rate_limit` | 见下表 | 分层 token bucket 容量与每秒回填速率 |
| `admin.header` | `Authorization` | 运维接口鉴权头 |
| `admin.token` | 未配置 | 运维 Bearer token；为空时运维路由不会注册 |
| `log.level` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `store.path` | `data/webhook.db` | SQLite 文件路径 |
| `store.retention` | `720h` | 终态事件、动作日志及心跳错误日志保留期；事件按 `processed_at`（缺省回退 `received_at`），心跳错误按 `created_at`；未完成事件不按此清理 |
| `store.retain_payload` | `false` | `false` 时，命中规则的事件在 `vars_json`/action 状态同事务提交后清除 payload；skipped 在终态提交后清除；预处理失败的 dead 事件保留 payload 供排障/replay |
| `queue.workers` | `4` | 异步处理 worker 数量（1–64） |
| `queue.buffer` | `256` | 内存唤醒信号容量，不是持久任务容量 |
| `queue.max_retries` | `2` | 首次执行后的额外重试次数（0–20）；0 表示只尝试一次 |
| `queue.retry_base` | `1s` | 指数退避基础时间 |
| `queue.max_retry_delay` | `15m` | 抖动退避上限；服务端 `Retry-After` 不会被静默截短 |
| `queue.max_retry_age` | `24h` | 从接收时间起算；派发前到期的 pending/retrying 动作进入 `dead`，包括尚未首试的动作；人工 replay 重新起算 |
| `heartbeat.interval` | `60s` | 死链心跳周期（10s–1h），也是对外监控的告警时间基准 |
| `heartbeat.timeout` | `10s` | 每个目标的采样与发送分别使用该预算（1s–30s，且不超过 interval）；SQL 错误落库另有最多 1s 预算 |
| `heartbeat.host` | `general-webhook` | 心跳 JSON 主机标识，最多 128 字节；每实例设置唯一 `HOST_LABEL` 和独立监控项 |
| `heartbeat.targets` | 空 | 最多 16 个独立目标；URL 全部为空时模块不启动 |

分层限流默认值：

| `server.rate_limit` 子项 | `burst` | `per_second` | 范围 |
| --- | --- | --- | --- |
| `pre_auth_per_ip` | 120 | 20 | 鉴权前按客户端 IP 保护 |
| `global` | 120 | 2 | 通过来源鉴权后的全局配额 |
| `source_per_ip` | 30 | 0.5 | 通过鉴权后的 source + IP 配额 |
| `admin_per_ip` | 60 | 1 | 管理接口独立 IP 配额 |

未认证请求不消耗 `global`；各字段省略或为 0 时采用默认值，不能用 0 关闭限流。`burst` 可设 1–100000，`per_second` 可设 0.01–100000。拒绝返回 429，并增加 `general_webhook_rate_limit_rejections_total{layer}`；layer 分别为 `pre_auth_ip`、`authenticated_global`、`source_ip`、`admin_ip`。生产者需对 429/503 进行有退避的重投，只有返回 202 的事件已写入 outbox。

### Source 配置

| 字段 | 说明 |
| --- | --- |
| `name` | 事件源名称，对应 `/webhook/{name}` |
| `auth` | 鉴权配置，必须显式声明 `type` |
| `extract` | 变量名到 gjson 路径的映射，支持 `$.a.b` 写法 |
| `rules` | 正则过滤规则，所有规则都命中才执行动作；空数组表示总是触发 |
| `actions` | 动作列表，按顺序执行 |
| `log_policy` | 可选出口 allowlist；拒绝时提交 skipped 后计数 |
| `redaction.patterns` | 可选脱敏正则；匹配的变量内容替换为 `***` |

`hmac` 和 `token` 类型必须配置至少 16 字节的 `secret`。GitHub relay 示例现在默认使用 `signed_headers: [timestamp, delivery_id, body]`，由 relay 先验证上游签名，再对 `timestamp + "\n" + delivery_id + "\n" + 原始 body` 使用本服务共享 secret 重签 HMAC-SHA256。

relay 必须发送 `X-Webhook-Timestamp`、`X-GitHub-Delivery`（或 `X-Delivery-Id`）及 `X-Hub-Signature-256: sha256=<hex>`，其中时间戳文本、delivery ID 和 body 必须与签名内容一致。**升级此示例需要同步升级 relay，不能直接转发 GitHub 原始 body-only 签名。** 若必须保留旧协议，显式配置 `signed_headers: []`；此模式未把时间戳与 delivery ID 绑定到签名，捕获合法 body/signature 的一方仍可能在 body 去重窗口之外换头重放。
示例配置默认包含 `github`、`custom`、`bt`、`dingtalk-demo` 四个 source；使用默认配置启动时，请提供其中 token/HMAC source 需要的 secret 环境变量，或删除暂不启用的 source。

**环境变量处理**：

- `auth.secret` / `admin.token` / heartbeat URL/token 的 `${VAR}`、`$VAR`、`${ENV:VAR}` 在配置加载时只展开原配置一次；替换值中的 `$` 或 `${...}` 保持原样。凭据不会进入动作快照。
- `${event:name}` 只读取事件变量；`${secret:NAME}` 在启动校验时必须存在、在 action 执行时读取环境变量，快照保存引用而非凭据。
- 旧式 `${name}` 为兼容语法：先读取事件变量，未命中时读取同名环境变量；新增配置应优先使用显式命名空间。

Compose 自身也会解析 `.env`。值含 `$` 时应使用单引号，例如 `CUSTOM_TOKEN='literal-$VALUE-keep-this'`，保留字面值后再交给应用。

示例：

```yaml
sources:
  - name: github
    auth:
      type: hmac
      secret: ${GITHUB_SECRET}  # 配置加载时从环境变量注入
      signed_headers: [timestamp, delivery_id, body]
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
| `headers` | ❌ | 自定义请求头，key/value 都支持插值；三个执行身份头由服务设置，见下文 |
| `body` | ❌ | 请求体模板，支持 `${var}` 插值；JSON 字符串值会做转义；为空时不设 body |
| `url_allowlist` | ❌ | 结构化 host、host:port、CIDR 或 URL 路径白名单；未配置时仅允许公网 `https` |
| `allow_private` | ❌ | 默认 `false`；私网 DNS/IP 目标必须同时显式启用并命中 allowlist |

默认 `Content-Type: application/json`（`body` 非空且未显式设置时），可通过 `headers` 覆盖。每次 HTTP action 尝试的总超时固定为 15 秒。初始 URL、DNS 解析结果及每次重定向都会校验；跨主机重定向、loopback、link-local、metadata 与 DNS rebinding 会被拒绝。出站请求不继承 `HTTP_PROXY`，如需代理应把经过评审的网关配置为明确目标。

目标路径与 URL 白名单采用相同的解码、归一化规则。编码分隔符、编码点段、反斜杠和可再次解码的百分号序列会被拒绝，例如 `/hooks/%2e%2e/admin`、`..%2fadmin`、`%252e%252e`；普通文字百分号路径如 `100%25` 仍可使用。重定向目标执行同样的路径检查。

队列执行 HTTP action 时，强制设置 `Idempotency-Key: <持久 event_id>:<action_index>`、`X-Webhook-Event-ID` 和 `X-Webhook-Attempt`，不能通过模板头覆盖。同一 action 的首次、重试、人工 replay 保持同一个幂等键；尝试次数独立表示，人工 replay 从 1 重新计数。下游仍需实际实现幂等处理。

**出站目标代理**：若容器访问 Telegram 等目标必须经过代理或兼容网关，可把 action 的 `url` 改为经过评审的出站地址，并把它加入 `url_allowlist`。它只改变 Webhook 发出的请求，不表示可以把本服务通过反向代理暴露到公网。

#### exec

执行本地命令或脚本。

```yaml
- type: exec
  command: "/opt/general-webhook/scripts/run.sh"
  args: ["${job}"]
  timeout: 300s
```

`exec` 不经过 shell，`command` 与 `args` 会逐个传入 `exec.CommandContext`；超时或关停取消时终止原进程组。默认超时 `60s`，最多保存 8 KiB 输出并附截断标记。进程取消或退出后，对遗留输出管道额外最多等待 250ms，避免后代一直持有 stdout/stderr 阻塞 worker。

调用 `setsid` 脱离原进程组的后代不保证被终止；需要整棵进程树的强制生命周期隔离时，应使用容器/cgroup 或专用任务执行器。脚本应自行清理后代并保证重复执行安全。

### 变量插值

动作配置里的字符串支持 `${event:var}`、`${secret:NAME}` 和兼容形式 `${var}`，用于：

- `http.url`
- `http.headers` 的 key 和 value
- `http.body`
- `exec.args`

变量优先来自当前 source 的 `extract` 配置；未命中时读取同名环境变量；仍不存在时为空字符串。HTTP JSON body 中的变量会按 JSON 字符串规则转义，避免消息里的引号、反斜杠或换行打坏 JSON。

### 日志出口策略与脱敏

可在单个 source 下配置：

```yaml
log_policy:
  mode: allowlist
  field: service
  services: [edge-nginx, app]
  on_reject: drop_and_count
redaction:
  patterns:
    - 'sk-[A-Za-z0-9]{16,}'
```

allowlist 先使用原始抽取值判断；缺少字段或值不在列表时拒绝，成功提交 `skipped` 后才增加计数。允许的事件再进行规则匹配和内容脱敏。拒绝日志、摘要及指标不记录拒绝原值；指标为 `general_webhook_policy_drops_total{source="...",reason="allowlist_rejected"}`，旧 `value` 标签已移除。

统计维度在启动时固定为配置来源及 `other` 汇总桶，旧快照里已移除的来源计入 `other`。心跳摘要最多列出 10 个来源，其余合并计数，总长度小于 1 KiB；计数是进程生命周期内的统计，重启清零。

脱敏后的变量先持久化到 `vars_json`，之后重试和 replay 原样恢复，包含消息中引用的 `delivery_id` 等值。HTTP 幂等头使用独立可信执行元数据（`ExecutionMetadata`），不会因内容脱敏而变化，也不会在重试时把已脱敏的变量恢复为原文。

## 心跳模块（Deadman Heartbeat）

心跳由 Go 进程主动 POST，默认面向 **Uptime Kuma 的 Push 监控项**。标准部署不发布宿主端口，因此 Kuma 通过收到的 up/down 和缺报期限判断状态，无需入站探测本服务。

```text
每个目标独立 worker，每轮最多一个请求
  -> 检查 server 未关停、queue worker 存活、SQLite 真实写入及统计查询
  -> 向 Kuma POST 原 push 路径，设置 query.status=up 或 down
  -> 仅 2xx 且 JSON ok:true 才确认投递成功
  -> 失败写 stdout + SQLite heartbeat_logs，并更新每目标指标
```

| `mode` | 健康 / 故障协议 | 成功确认 |
| --- | --- | --- |
| `kuma`（默认） | 保留 push 路径和其它 query，覆盖 `status=up/down`、`msg=OK/not ready` | HTTP 2xx 且响应 JSON `ok: true` |
| `healthchecks` | 健康使用原路径；故障在 URL 路径末尾追加 `/fail`，保留 query | HTTP 2xx |
| `push`（旧别名） | 与 `healthchecks` 相同 | HTTP 2xx |
| `json` | 始终 POST 原 URL，由负载 `status: pass/fail` 表示状态 | HTTP 2xx |

所有协议都发送 JSON，字段包括 `host`、`status`、`exit_code`、`timestamp`、`uptime_seconds`、`revision`、可读取的 `stats` 和有界 `drop_summary`。Kuma 实际按 query 判断状态，不能只依赖 JSON 的 `status`。自建 JSON 接收器需自行把 pass/fail 接入通知规则。

最多配置 16 个目标，各有独立 ticker 和 worker；一个目标的慢网络请求不会串行阻塞其它目标。设 `heartbeat.timeout = T`，健康采样和 HTTP 发送各有 T 预算，SQL 错误落库使用独立的 `min(T, 1s)` 预算，单目标一轮最多 `2T + min(T, 1s)`。采样超时仍会用新的发送预算主动报告 down；同一目标不重叠发送，慢轮次可能拉长其实际发送间隔。

`up` 表示本地可接收工作的就绪检查通过：worker 存活、服务未关停、数据库能够提交真实写入且统计查询成功。动作下游的交付质量、`dead` 数、失败率、队列积压与 worker 进展需另外监控；一次成功的 down 投递也会刷新 `LastSuccess`，该指标衡量投递新鲜度。

URL 默认必须使用 HTTPS，长度最多 4096 字节，禁止 userinfo 和 fragment；不继承 HTTP 代理，也不跟随重定向，3xx 会计为失败。内部 HTTP Kuma 必须显式配置 `allow_http: true`。可选 `token` 放入 `X-Heartbeat-Token`，Kuma 一般只需要 URL 中的 push token。

每实例使用独立的 Kuma 监控项和唯一 `HOST_LABEL`（最多 128 字节）。多个实例共享同一 push URL 时，仍存活的实例可能持续为其它实例续期。首个心跳在 `interval` 后发送；监控宽限应覆盖 interval、最坏单轮耗时和预期启动/重启时间。`Stop` 会取消并等待正在进行的心跳，正常关停不额外发送 down。

在 Kuma 创建 Push 监控项，复制它生成的完整 URL 到 `.env`：

```dotenv
HEARTBEAT_URL='https://kuma.example.com/api/push/<token>?status=up&msg=OK&ping='
HEARTBEAT_TOKEN=
HOST_LABEL=tokyo-webhook
```

URL 全部留空时心跳关闭；只修改 URL/token/host 环境值后重新创建容器即可。若使用 `http://uptime-kuma:3001/...`，还需让两个服务经受控网络互通，并修改镜像内配置后重建自己的唯一标签镜像：

```yaml
heartbeat:
  targets:
    - name: kuma
      url: ${HEARTBEAT_URL}
      mode: kuma
      allow_http: true
```

继续使用 Healthchecks 时，显式设 `mode: healthchecks`（或保留已有 `mode: push`），UUID 地址应为 `https://hc-ping.com/<check-uuid>`。省略 mode 的旧配置升级后会使用 Kuma 协议，必须检查并补齐。JSON 接收器继续使用 `mode: json`，可沿用 `CF_HEARTBEAT_URL` / `CF_HEARTBEAT_TOKEN`。

push 失败会记录安全目标名称、origin 与错误类别，不记录 URL path/query、token 头或响应体。网络错误只输出分类结果，避免嵌套 `url.Error` 把完整 URL 带回日志。数据库不可写或落库预算耗尽时，保留 stdout 错误并增加 `general_webhook_heartbeat_target_log_failures_total`，不会递归尝试记录数据库错误。成功推送的 down 属于正常投递，不写 push 错误行。

## 审计与日志

SQLite 会自动创建持久状态与审计表：

| 表 | 内容 |
| --- | --- |
| `events` | source、状态、`vars_json`、毫秒时间、replay 代次和动作快照；按要求使用 `${secret:NAME}` 时只保存引用 |
| `event_actions` | 每个 action 的状态、尝试次数、`next_attempt_at`、最近错误与完成时间 |
| `action_logs` | 每次尝试的 action index、replay 代次、已脱敏 target/detail 和耗时 |
| `replay_guard` | HMAC body 的原子、带过期时间重放保护键 |
| `heartbeat_logs` | 失败 push 的 target、origin、mode、health、error、http_status、duration_ms、created_at；不保存 URL path/query、token 头或响应体 |

日志输出到 stdout，格式为 JSON。标准 Compose 使用有界的 Docker `json-file` 日志（10 MiB × 5）；若另接 Loki/ELK，应保留等价的容量或保留期边界。日志以及写入 SQLite 的 action target/detail 都会先脱敏；HTTP target 只保留 `scheme://host[:port]`。

SQLite 使用 WAL、`synchronous=FULL` 和 250ms `busy_timeout`，单实例通过一个连接串行访问；readiness 和心跳审计写入还会根据剩余 context 预算缩短锁等待。数据目录权限为 `0700`，数据库及 `-wal` / `-shm` 文件为 `0600`。`store.retention` 默认保留终态事件与心跳错误 30 天，未完成事件仍需监控磁盘容量。

备份应停机后复制整个数据目录，或使用 SQLite 备份 API；运行中只复制 `.db` 可能遗漏 WAL 中已提交的数据。启动时清理旧 action 快照采用稳定主键分页，每批最多 128 行和 1 MiB；单条超过预算的快照单独处理，峰值还受最大单条大小影响。操作可重试，不会一次载入全部历史快照。

HMAC 的同 source/body replay guard 与事件接收同事务提交；时间戳直接按允许窗口校验，避免远未来时间值的溢出。动作凭据使用 `${secret:NAME}` 引用；HTTP action 的 `4xx`（除 408/429）不重试，并持久化 `Retry-After`。配置中的当前凭据可在启动时从旧快照移除，但历史备份需独立处理和按需轮换凭据。

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

- 推荐部署：GHCR 镜像 + Docker Compose；定制时使用 Dockerfile 构建唯一标签，见 [INSTALL.md](INSTALL.md) 与 [deploy/README.md](deploy/README.md)。
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
- 外部命令有超时和额外 250ms 的输出管道等待限制，输出会截断后再进入审计记录。
- HTTP 动作未配置 `url_allowlist` 时仅允许公网 HTTPS；配置后目标必须命中，私网目标还必须显式设置 `allow_private: true`。
- 运维接口只有配置 `admin.token` 后才注册；默认 `Authorization` header 要求 Bearer token，并有独立限流。
- 使用 `${secret:NAME}` 的动作凭据在快照中只保存引用；启动时按有界批次清理旧快照中仍可识别的当前凭据值。
- 日志以及写入审计库的 action target/detail 会在持久化前脱敏。
- SQLite 使用单连接和限时锁等待；外部写入竞争可能返回错误，生产者应按返回状态重投。

## 运维接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/events/{id}` | 事件状态与近期 action_logs |
| `GET` | `/queue/stats` | 队列状态计数与最老未完成事件年龄 |
| `GET` | `/heartbeat/logs?limit=20` | 最近 push 错误；limit 默认 20，可设 1–100，超范围返回 400 |
| `POST` | `/events/{id}/retry` | 只重置 `dead` action；成功 action 不会重放 |
| `GET` | `/metrics` | Prometheus 文本指标 |

这些接口与 webhook 共用监听地址，同时受 Docker 网络和管理 token 两层保护。默认 `admin.header: Authorization` 时必须携带 `Authorization: Bearer $ADMIN_TOKEN`；若改为自定义 header，则直接传原始 token，不加 `Bearer`。未配置 token 时路由返回 404。以下 curl 应从已加入受控网络的运维容器执行；本地调试时可把地址换为 loopback。replay 可选提交不超过 256 字节的审计原因：

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://general-webhook:8080/queue/stats
curl -H "Authorization: Bearer $ADMIN_TOKEN" 'http://general-webhook:8080/heartbeat/logs?limit=20'
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"reason":"downstream recovered"}' http://general-webhook:8080/events/EVENT_ID/retry
```

旧版本 `partial` / `error` 事件没有 `event_actions` 明细时，人工 replay 会初始化并重放该事件的全部 action；操作前必须确认下游幂等性。新版本 `dead` 事件只重置失败 action，已经 `done` 的 action 保持不变；`max_retry_age` 从本次 replay 时间重新计算。

`/heartbeat/logs` 返回 `{"heartbeat_errors":[...]}`，按时间及记录 ID 倒序。每条包含 `id`、`target`、`origin`、`mode`、`health`（pass/fail）、`error`、`http_status`（未收到响应时为 0）、`duration_ms` 与 UTC `created_at`，不包含 push URL path/query、token 头或远端响应体。数据库不可用时该管理查询返回 503，push 错误的备用记录见 stdout 和 `log_failures` 指标。

`/metrics` 暴露事件/动作队列深度、最老未完成事件年龄，以及 5 分钟和 1 小时滚动 action 尝试数与失败率。至少应对以下条件告警：

- `general_webhook_events{status=~"dead|partial|error"} > 0`
- `general_webhook_oldest_pending_age_milliseconds` 超过业务允许的未完成时限
- `general_webhook_action_failure_ratio{window="5m"}` 持续升高（同时用 `general_webhook_action_attempts` 设置最小样本量）
- 每目标 `general_webhook_heartbeat_target_failures_total` 持续增加，或 `general_webhook_heartbeat_target_last_success_timestamp` 超过允许宽限（按 `target` / `mode` 标签区分）
- `general_webhook_heartbeat_target_log_failures_total` 增加，表示对应目标的 push 错误未成功写入 SQL
- `general_webhook_rate_limit_rejections_total{layer}` 持续增加，需检查发送速率及生产者重投

另有每目标 `general_webhook_heartbeat_target_attempts_total` 和 `general_webhook_heartbeat_target_last_duration_milliseconds`。全局 `general_webhook_heartbeat_last_success_timestamp` 表示任一目标接受 push，不应代替每目标监控；成功接受 down 也会更新成功时间。计数器重启清零，首个心跳前时间指标为 0，应结合启动宽限处理。

## 当前限制

- SQLite outbox 是单节点持久队列，不是分布式队列；单实例重启会重置本进程遗留的 `processing` 租约。
- `retain_payload=false`（默认）时，命中规则的事件在 `vars_json` 和 action 状态提交成功后清除 payload，skipped 事件在提交终态后清除；预处理失败的 dead 事件通常保留 payload。已持久化变量的 dead replay 不依赖 payload；旧版本已清空且没有 `vars_json` 的事件会明确返回 `409`。
- 规则正则使用最多 256 项的 FIFO 编译缓存，单个可缓存表达式最多 4 KiB；更大的合法表达式仍可执行，但不会驻留缓存。
- `exec.command` 由镜像内配置控制，目前没有额外的允许目录白名单。
- 交付为可重试语义：远端已经产生副作用、但本地尚未提交成功状态时崩溃，可能再次执行。HTTP 下游应消费固定幂等键；exec 需要业务幂等和后代清理。
- `restart: unless-stopped` 只在进程退出时重启；单纯 `unhealthy` 不会触发 Docker 自动重启。配置 URL 启用心跳后，进程消失由远端缺报告警，本地不就绪通过 Kuma down / JSON fail 表达；默认 URL 为空时心跳关闭。如需自动重启，应另行部署相应的宿主机管理机制。
- 心跳依赖独立监控端。宿主机断网或断电时，由远端监控的缺报期限告警；监控端自身及其通知通道也需要独立监控。

## 后续方向

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
