# Deploy

正式部署入口是仓库根目录的 `Dockerfile`、`docker-compose.yaml` 与 [INSTALL.md](../INSTALL.md)。`sync-to-opt.sh` 是可选的主机准备助手：它同步构建上下文、准备 SQLite 数据目录、创建或验证 Docker internal 入口网络，再打印显式加载目标 `.env` 的 Compose 命令。

- 默认应用目录：`/opt/general-webhook`
- 默认数据目录：`/data/webhook`
- 固定容器 UID/GID：`10001:10001`
- 标准部署文档：[INSTALL.md](../INSTALL.md)

脚本必须以 root 运行；`CAPTAIN_USER` 必须能访问 Docker daemon，因此属于 root 等级的受信任运维账号。若目标 `.env` 已存在，重新运行时必须通过环境变量传入与其中一致的 `WEBHOOK_DATA_DIR`、`WEBHOOK_UID`、`WEBHOOK_GID` 和 `WEBHOOK_INTERNAL_NETWORK`，否则脚本会拒绝继续，避免准备错误的数据目录或入口网络。`WEBHOOK_IMAGE` 必须使用唯一且非 `latest` 的版本标签，以便作为本地 Dockerfile 构建输出。

`general-webhook.service` 与 `general-webhook.env.example` 是历史 systemd 兼容文件，不属于标准生产部署流程，不保证与 Docker 配置保持功能等价。
