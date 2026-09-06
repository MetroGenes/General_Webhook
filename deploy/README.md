# Deploy

正式部署入口是仓库根目录的 `Dockerfile`、`docker-compose.yaml` 与 [INSTALL.md](../INSTALL.md)。

默认使用 GHCR 预构建镜像，通过 Compose 运行，不发布宿主机端口。按 [INSTALL.md](../INSTALL.md) 准备 `.env`、数据目录和入口网络后：

```bash
docker compose --env-file .env config -q
docker compose --env-file .env pull webhook
docker compose --env-file .env up -d --no-build --remove-orphans --wait --wait-timeout 60
```

`.env.example` 默认选择 `ghcr.io/metrogenes/general_webhook:1.0.0`，支持 `linux/amd64` 和 `linux/arm64`。`1.0.0` 与 `latest` 是主分支通过 CI 后更新的部署标签；需要固定版本或回滚时，保存对应 `sha-<完整提交 SHA>` 或镜像 digest。旧 Git `v1.0.0` 标签保持原提交。

配置和脚本已烘焙进镜像。修改 `configs/webhooks.yaml`、`scripts/`，或为内部 HTTP Kuma 开启 `allow_http: true` 时，先把 `WEBHOOK_IMAGE` 改为自己的唯一标签，再执行 `docker compose --env-file .env build --pull` 和上述 `up --no-build`。本地构建可用 `GENERAL_WEBHOOK_GIT_COMMIT` 设置 `VCS_REF`；官方镜像自带提交标识。

心跳默认使用 Uptime Kuma Push 协议，错误保存在 SQLite `heartbeat_logs`，可经管理鉴权访问 `/heartbeat/logs?limit=20`。协议、升级与备份说明见 [README](../README.md#心跳模块deadman-heartbeat) 和 [INSTALL.md](../INSTALL.md)。
