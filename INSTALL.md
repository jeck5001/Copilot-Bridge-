# 完整安装与运维指南

本文不假定任何既有服务器、域名、代理或集群。请选择适合自己的方式：本机源码运行、本机 Docker、服务器 Docker 或服务器二进制服务。

## 1. 运行要求

### 源码方式

- Go 1.27+
- Git（也可直接下载源码压缩包）
- 可访问 Microsoft 登录和 Microsoft 365 Copilot 相关上游服务的网络
- 一个你有权使用的 Microsoft 365 Copilot 账号

### Docker 方式

- Docker Engine 24+
- Docker Compose v2
- 建议至少 1 vCPU、512 MiB 可用内存和 1 GiB 可用磁盘

## 2. 本机 Docker 安装（推荐）

```bash
git clone https://github.com/vipamess/Copilot-Bridge-.git
cd Copilot-Bridge-
docker compose up -d --build
docker compose ps
```

浏览器访问 <http://127.0.0.1:4141>，使用初始密码 `admin888` 登录并按要求修改密码。

默认 Compose 只把端口发布到本机回环地址。账号、密钥和会话保存在 Docker 命名卷中，不会写回源码目录。

常用命令：

```bash
# 查看日志
docker compose logs -f --tail=200 gateway

# 重启
docker compose restart gateway

# 停止但保留数据
docker compose down

# 更新源码并重建
git pull --ff-only
docker compose up -d --build
```

## 3. Windows 源码安装

安装 Go 1.27+ 后，在 PowerShell 执行：

```powershell
git clone https://github.com/vipamess/Copilot-Bridge-.git
Set-Location .\Copilot-Bridge-
go version
.\scripts\run-local.ps1
```

该脚本会：

1. 创建 `data` 目录；
2. 将账号、会话、密钥、设置和日志路径固定在该目录；
3. 默认绑定 `127.0.0.1:4141`；
4. 在未设置管理员引导密码时使用 `admin888`；
5. 执行 `go run ./cmd/server`。

也可以编译单文件：

```powershell
New-Item -ItemType Directory -Path .\bin -Force | Out-Null
go build -trimpath -o .\bin\m365-gateway.exe .\cmd\server
.\scripts\run-local.ps1 -Binary .\bin\m365-gateway.exe
```

停止服务请在当前窗口按 `Ctrl+C`，程序会执行优雅退出并落盘统计。

## 4. Linux/macOS 源码安装

```bash
git clone https://github.com/vipamess/Copilot-Bridge-.git
cd Copilot-Bridge-
go version
chmod +x scripts/run-local.sh
./scripts/run-local.sh
```

编译后运行：

```bash
mkdir -p bin
go build -trimpath -o bin/m365-gateway ./cmd/server
./scripts/run-local.sh ./bin/m365-gateway
```

## 5. 首次初始化

服务首次启动不会自带任何账号或 API Key。

1. 打开管理页面。
2. 用 `admin888` 登录。
3. 设置至少 12 个字符、且不等于 `admin888` 的新密码。
4. 重新登录。
5. 进入“添加账号”，开始 OAuth PKCE 授权。
6. 登录完成后复制浏览器地址栏的完整回调地址，粘贴回页面提交。
7. 在账号列表确认状态；如无特殊需要保持“默认直连”。
8. 进入“访问配置”，创建永久或限期 API Key。
9. 立即保存完整密钥；服务端之后只保留哈希，无法再次显示原文。

## 6. 客户端接入

通用 OpenAI 兼容配置：

```text
Base URL = http://127.0.0.1:4141/v1
API Key  = m365_...
Model    = gpt-5.6-sol
```

如果客户端要求“OpenAI API URL”，通常填写到 `/v1`；如果客户端要求完整端点，则填写 `/v1/chat/completions` 或 `/v1/responses`。

快速检查：

```bash
export M365_GATEWAY_API_KEY='paste-your-created-key-here'
curl http://127.0.0.1:4141/api/health
curl http://127.0.0.1:4141/v1/models \
  -H "Authorization: Bearer ${M365_GATEWAY_API_KEY}"
```

## 7. 任意服务器上的 Docker 部署

以下示例适用于你自己选择的 Linux 服务器，不依赖服务器编号或固定拓扑。

```bash
git clone https://github.com/vipamess/Copilot-Bridge-.git
cd Copilot-Bridge-
cp .env.example .env
docker compose up -d --build
```

建议保持 `.env`：

```dotenv
M365_BIND_ADDRESS=127.0.0.1
M365_PORT=4141
M365_ADMIN_PASSWORD=admin888
M365_COOKIE_SECURE=true
```

先通过 SSH 端口转发完成初始化最安全：

```bash
ssh -L 4141:127.0.0.1:4141 your-user@your-server
```

然后在本机浏览器打开 <http://127.0.0.1:4141>，修改初始密码、添加账号和创建 API Key。

## 8. HTTPS 反向代理

生产网络中不要直接公开 4141。先准备自己的域名和证书，再选 Nginx 或 Caddy。以下示例不包含任何预设域名或 IP。

### Nginx

```nginx
server {
    listen 443 ssl http2;
    server_name gateway.example.com;

    ssl_certificate     /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    client_max_body_size 8m;
    proxy_connect_timeout 30s;
    proxy_send_timeout  650s;
    proxy_read_timeout  650s;

    location / {
        proxy_pass http://127.0.0.1:4141;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
    }
}
```

### Caddy

```caddyfile
gateway.example.com {
    reverse_proxy 127.0.0.1:4141 {
        flush_interval -1
        transport http {
            read_timeout 650s
            write_timeout 650s
        }
    }
}
```

HTTPS 部署必须设置 `M365_COOKIE_SECURE=true`。确认公网防火墙只开放 80/443，并让 4141 保持仅回环可达。

## 9. Linux systemd 二进制部署

先编译：

```bash
go build -trimpath -ldflags='-s -w' -o m365-gateway ./cmd/server
sudo install -m 0755 m365-gateway /usr/local/bin/m365-gateway
sudo useradd --system --home /var/lib/m365-gateway --shell /usr/sbin/nologin m365-gateway || true
sudo install -d -o m365-gateway -g m365-gateway -m 0700 /var/lib/m365-gateway
sudo install -d -o root -g root -m 0755 /opt/m365-gateway/web
sudo cp -a web/. /opt/m365-gateway/web/
```

创建 `/etc/systemd/system/m365-gateway.service`：

```ini
[Unit]
Description=M365 Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=m365-gateway
Group=m365-gateway
WorkingDirectory=/opt/m365-gateway
ExecStart=/usr/local/bin/m365-gateway
Restart=on-failure
RestartSec=5
Environment=M365_LISTEN=127.0.0.1:4141
Environment=M365_TOKEN_CACHE=/var/lib/m365-gateway/accounts.json
Environment=M365_SESSION_CACHE=/var/lib/m365-gateway/sessions.json
Environment=M365_API_KEYS=/var/lib/m365-gateway/api-keys.json
Environment=M365_SETTINGS_FILE=/var/lib/m365-gateway/settings.json
Environment=M365_DEBUG_LOG=/var/lib/m365-gateway/debug-logs.jsonl
Environment=M365_ADMIN_PASSWORD_HASH_FILE=/var/lib/m365-gateway/admin-password.hash
Environment=M365_ADMIN_PASSWORD=admin888
Environment=M365_COOKIE_SECURE=true
Environment=M365_LOG_LEVEL=warn
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/m365-gateway
CapabilityBoundingSet=
AmbientCapabilities=
LockPersonality=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
```

启用：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now m365-gateway
sudo systemctl status m365-gateway
sudo journalctl -u m365-gateway -f
```

## 10. 数据备份与恢复

运行数据包含有效令牌，应先停止写入并加密保存。

Docker 备份：

```bash
docker compose stop gateway
docker run --rm \
  -v m365-gateway-data:/source:ro \
  -v "$PWD":/backup \
  alpine:3.23 tar -czf /backup/m365-gateway-data.tar.gz -C /source .
docker compose start gateway
```

恢复到新建命名卷：

```bash
docker compose down
docker volume create m365-gateway-data
docker run --rm \
  -v m365-gateway-data:/target \
  -v "$PWD":/backup:ro \
  alpine:3.23 sh -c 'cd /target && tar -xzf /backup/m365-gateway-data.tar.gz'
docker compose up -d
```

本地源码方式只需在服务停止后，加密备份项目的 `data/`。不要把备份上传到 GitHub、网盘公开链接或问题附件。

## 11. 升级与回滚

升级前：

1. 备份数据卷或 `data/`；
2. 记录当前版本和提交 ID；
3. 阅读 `CHANGELOG.md`；
4. 在非生产环境验证账号登录、文本流、工具调用和长响应。

Docker 更新：

```bash
git fetch --all --tags
git pull --ff-only
docker compose build --pull
docker compose up -d
docker compose ps
```

回滚代码时不要回滚或覆盖数据目录。先切换到已知提交，再重建镜像；若数据格式发生不兼容，使用升级前的加密备份恢复。

## 12. 卸载

保留数据：

```bash
docker compose down
```

永久删除 Docker 数据前请确认已有备份：

```bash
docker compose down -v
```

源码方式停止服务后删除程序目录即可；若使用默认用户配置目录，还需由用户自行确认并删除 `~/.config/m365-gateway`。不要执行针对主目录或磁盘根目录的递归删除命令。

## 13. 常见故障

### 登录后仍要求修改密码

这是初始密码保护机制。使用当前密码 `admin888` 设置至少 12 个字符的新密码，然后重新登录。仅重启服务不会解除该限制。

### `401 valid API key required`

管理密码不是 API Key。请在“访问配置”创建 `m365_...` 密钥，并用 `Authorization: Bearer ...` 发送。

### `ws dial`、`ws read before completion` 或 `i/o timeout`

依次检查：当前账号是否在线、上游站点是否可达、DNS/TLS 是否被拦截、每账号代理是否有效、反向代理读取超时是否大于任务时间。不要在原因不明时高频重试所有账号；先查看管理页诊断详情中的错误分类和请求 ID。

### `429` 或 quota exhausted

这通常是账号或上游配额状态。网关会隔离当前失败账号并按固定顺序选择下一个健康账号，但不会保证绕过任何上游限制。降低并发、等待冷却、检查账号许可与配额。

### 流式响应被截断

确认客户端、CDN 和反向代理都关闭响应缓冲，并将读取超时配置到高于 `M365_CHAT_TIMEOUT_SECONDS`。优先直连测试，以区分网关、代理和边缘网络问题。

### 语音 Realtime 连接失败

本项目不实现 OpenAI Realtime 音频协议。`/v1/realtime` 会明确返回 501；请使用文本 Chat/Responses 流式接口。

更多配置见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)，安全问题见 [SECURITY.md](SECURITY.md)。
