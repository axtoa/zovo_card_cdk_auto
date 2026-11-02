# GHCR 镜像部署

GitHub Actions 编译前端与 Go/SQLite 后端，发布到
`ghcr.io/axtoa/zovo_card_cdk_auto`。服务器仅下载镜像，不需要 Node.js、Go 或源码。
支持 `linux/amd64` 与 `linux/arm64`；Docker 自动选择本机架构。

## 镜像发布

`.github/workflows/container.yml` 在 `vMAJOR.MINOR.PATCH` 标签推送时构建正式镜像。
普通 master 提交不构建镜像，也不会改变服务器使用的 `latest`。
使用仓库自带的 `GITHUB_TOKEN`，工作流声明 `packages: write`，不需要额外保存 PAT。
例如推送 `v1.4.35` 会在两个架构健康检查通过后发布 `v1.4.35`、`1.4.35`、
`latest` 和 `sha-<完整提交 SHA>`。每次发布使用新的版本号，不移动已发布标签。
也可在 Actions 页手动运行：选 master 只发布 SHA 镜像供测试，选正式版本标签则发布该版本。

发版时在开发机器的仓库中执行（以下版本号仅为示例）：

```bash
git push origin master
git tag -a v1.4.35 -m 'Release v1.4.35'
git push origin v1.4.35
```

等 Container image 工作流完成，再到服务器拉取。发布镜像不会自动更新正在运行的容器。

如果是 fork，先在 GitHub Actions 页面启用工作流。镜像第一次发布后，在 GitHub
个人主页 Packages → zovo_card_cdk_auto → Package settings 检查可见性。
**公开源码不保证镜像公开**；公开镜像可以匿名拉取。
私有镜像需在服务器运行 `docker login ghcr.io -u axtoa`，交互输入具有
`read:packages` 权限的 PAT（不要将令牌写进命令或发到聊天中）。

## 新服务器安装

要求 Docker Engine 和 Compose v2；域名 A 记录指向服务器公网 IPv4，若存在 AAAA，
它也必须指向本机可达的 IPv6。80/TCP、443/TCP 需放行且未被其他服务占用，
443/UDP 可选，用于 HTTP/3。若使用 Cloudflare，首次部署可先使用仅 DNS 模式。

在服务器执行：

```bash
mkdir -p /opt/axtoa/zovocdk
cd /opt/axtoa/zovocdk
curl -fL --retry 3 -o compose.yaml https://raw.githubusercontent.com/axtoa/zovo_card_cdk_auto/master/deploy/compose.image.yml
curl -fL --retry 3 -o Caddyfile https://raw.githubusercontent.com/axtoa/zovo_card_cdk_auto/master/deploy/Caddyfile.image
curl -fL --retry 3 -o init-image.sh https://raw.githubusercontent.com/axtoa/zovo_card_cdk_auto/master/deploy/init-image.sh
bash init-image.sh
docker compose pull
docker compose up -d
docker compose ps
```

下载到已存在目录前先备份 compose.yaml / Caddyfile / init-image.sh 中的自定义修改。
初始化脚本只在 `.env` 不存在时生成随机密钥，重复运行不会替换现有凭证。
`SITE_DOMAIN` 默认为 `shop.aisale.best`，需要时可在首次启动前修改 `.env`。

打开 `https://shop.aisale.best/ops/setup`，安装令牌从服务器读取：

```bash
sed -n 's/^SETUP_BOOTSTRAP_TOKEN=//p' .env
```

通过向导新建管理员。随后登录 `/ops/login`，配置卡台 API、Webhook 等。
回调地址为 `https://shop.aisale.best/api/v1/webhooks/cardplatform`。
本地预览的管理员不会自动出现在生产数据库里。

## 更新、停止与备份

```bash
cd /opt/axtoa/zovocdk
docker compose pull cdk
docker compose up -d --no-deps --wait cdk
docker compose ps
docker compose logs --tail=100 cdk
```

上面命令仅更新应用，保留现有 Caddy。重新创建应用容器期间会短暂中断服务。
现有 `.env`、`data/`、`caddy/` 保持不变，不需要重新初始化管理员或卡台配置。
如果 `.env` 中 APP_IMAGE 已固定到版本或摘要，需要先改成目标版本或摘要再拉取；
`latest` 模式则每次取最新正式发布。例如固定到已发布的 1.4.35：

```bash
sed -i 's|^APP_IMAGE=.*|APP_IMAGE=ghcr.io/axtoa/zovo_card_cdk_auto:1.4.35|' .env
docker compose pull cdk
docker compose up -d --no-deps --wait cdk
```

建议上线稳定后将 `.env` 的 `APP_IMAGE` 改成工作流日志中镜像检查步骤输出的
`ghcr.io/axtoa/zovo_card_cdk_auto@sha256:...`，避免标签移动导致版本不明确。
需要回退时填回旧镜像摘要，并确认旧程序兼容当前数据库结构或恢复对应备份。
Docker 镜像部署关闭了后台二进制热更新，统一通过镜像更新。

公开镜像允许匿名下载程序，不授予上传或覆盖镜像的权限。生产运行时的
`.env`、数据库和证书保留在服务器挂载目录，不会因为发布镜像而公开。
Docker 构建上下文已排除 `.env*`、`data/`、`caddy/`、数据库文件和本地密钥文件。

数据库和证书持久化在当前目录的 `data/` 与 `caddy/`；`docker compose down`
停止容器但保留这些目录。`.env`、`data/`、`caddy/` 均包含敏感信息，不要上传 Git。
一致性备份可短暂停站后打包：

```bash
docker compose stop
tar -czf "backup-$(date +%Y%m%d-%H%M%S).tgz" .env data caddy compose.yaml Caddyfile
docker compose start
```

初始资源限制：应用 256 MiB，Caddy 128 MiB。这是内存上限，不是预留量；
不能保证所有负载下都足够。1 GB 主机适合低访问量起步，512 MB 主机余量很小。
出现 OOM 时检查 `docker stats` / `docker inspect`，再升级内存或调整上限。
交换空间只有宿主机实际配置后才可用，Compose 的 memswap_limit 不会创建 swap。
