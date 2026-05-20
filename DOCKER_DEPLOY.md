# Docker 部署说明

本文档适用于把当前仓库部署到 Linux VPS。项目已经包含 `Dockerfile` 和 `docker-compose.yml`，推荐使用 Docker Compose 部署。

## 1. 环境要求

- Linux VPS，建议 Debian、Ubuntu、Alpine、Rocky Linux 等常见发行版
- Docker Engine
- Docker Compose v2，即 `docker compose` 命令
- 服务器安全组/防火墙已放行面板端口和所有 Xray 节点端口

不建议在 Windows 或 macOS 的 Docker Desktop 上正式运行。当前 Compose 使用 `network_mode: host`，正式代理服务更适合 Linux 宿主机网络。

## 2. 拉取代码

```bash
git clone https://github.com/xzy593232500/3X-UI.git
cd 3X-UI
```

如果服务器上已经有仓库：

```bash
cd 3X-UI
git pull
```

## 3. 准备持久化目录

```bash
mkdir -p db cert
```

目录用途：

- `./db`：面板数据库和运行配置，挂载到容器内 `/etc/x-ui/`
- `./cert`：面板 TLS 证书目录，挂载到容器内 `/root/cert/`

只要这两个目录保留，删除或重建容器不会丢失面板数据。

## 4. 可选：创建 Docker 环境变量文件

默认情况下可以直接运行，不需要额外配置。

如果想修改镜像名、数据目录、时区或 fail2ban，可以复制示例文件：

```bash
cp .env.docker.example .env.docker
nano .env.docker
```

使用 `.env.docker` 启动时：

```bash
docker compose --env-file .env.docker up -d --build
```

不使用 `.env.docker` 时：

```bash
docker compose up -d --build
```

## 5. 启动服务

```bash
docker compose up -d --build
```

首次构建会下载 Go 依赖、Xray 内核和 geo 数据文件，需要等待几分钟。

查看容器状态：

```bash
docker compose ps
```

查看日志：

```bash
docker compose logs -f
```

## 6. 访问面板

默认面板端口通常是 `2053`：

```text
http://你的服务器IP:2053/
```

如果客户所在网络不方便访问非常规端口，可以在服务器 Nginx 上把 `/customer-sub/`
反代到面板端口，并在 `.env.docker` 中设置公开订阅地址：

```env
XUI_PUBLIC_SUB_BASE_URL=http://你的服务器IP
```

设置后，客户订阅页面生成的客户订阅链接会优先使用这个公开地址。

默认账号密码通常是：

```text
admin / admin
```

首次登录后请立刻修改：

- 面板用户名
- 面板密码
- 面板路径
- 面板端口

如果你在面板中修改了端口或节点端口，需要同步放行服务器防火墙和云厂商安全组。

## 7. 更新部署

```bash
cd 3X-UI
git pull
docker compose up -d --build
```

更新后确认日志：

```bash
docker compose logs -f --tail=100
```

## 8. 停止、重启、删除容器

停止：

```bash
docker compose stop
```

重启：

```bash
docker compose restart
```

删除容器但保留数据：

```bash
docker compose down
```

注意：不要删除 `./db` 和 `./cert`，否则会丢失面板数据或证书。

## 9. 备份与恢复

备份：

```bash
tar -czf 3x-ui-backup-$(date +%F).tar.gz db cert
```

恢复：

```bash
tar -xzf 3x-ui-backup-YYYY-MM-DD.tar.gz
docker compose up -d --build
```

## 10. 常见问题

### 无法访问面板

检查容器是否运行：

```bash
docker compose ps
docker compose logs --tail=100
```

检查服务器安全组、防火墙是否放行面板端口。

### 节点端口无法连接

当前 Compose 使用 host 网络，不需要在 `docker-compose.yml` 中配置 `ports`。你需要在服务器防火墙和云厂商安全组里放行节点端口。

### 构建时下载失败

构建过程会从 GitHub、Go 模块代理下载依赖。网络不稳定时可以重新执行：

```bash
docker compose build --no-cache
docker compose up -d
```

### fail2ban 不需要时如何关闭

复制 `.env.docker.example`：

```bash
cp .env.docker.example .env.docker
```

把下面这一行改成：

```text
XUI_ENABLE_FAIL2BAN=false
```

然后启动：

```bash
docker compose --env-file .env.docker up -d --build
```
