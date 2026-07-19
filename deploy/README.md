# Deploy

正式部署入口是仓库根目录的 `Dockerfile`、`docker-compose.yaml` 与 [INSTALL.md](../INSTALL.md)。

本项目**唯一**推荐的生产部署方式：

1. 用根目录 `Dockerfile` 构建镜像
2. 用 `docker-compose.yaml` 运行（不发布宿主机端口）

不要使用裸机二进制、systemd 单元或宿主机同步脚本。完整步骤见 [INSTALL.md](../INSTALL.md)。
