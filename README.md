# M365 Gateway

独立维护的 Microsoft 365 Copilot 兼容网关。它把已获授权的 Microsoft 365 Copilot 账号接入转换为 OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 风格的 HTTP API，并提供独立的 Web 管理控制台。

> **免责声明：本项目仅供学习、研究和互操作性测试。** 它不是 Microsoft、OpenAI 或 Anthropic 的官方产品，与上述公司不存在隶属、授权或背书关系。仅可连接你有权使用的账号、租户和网络资源；使用者应自行核对适用法律、组织政策与服务条款，并自行承担账号、数据、配额及服务可用性风险。详见 [DISCLAIMER.md](DISCLAIMER.md)。

## 版本状态

这是一个全新初始化的公开发行版：

- 不包含任何账号、邮箱、OAuth 令牌、API Key、代理地址、域名、IP、证书或运行日志。
- 不包含任何特定服务器编号、集群拓扑、生产备份或运维凭据。
- 首次运行时账号池、API Key、会话、统计和日志均为空。
- 用户可以在本机运行，也可以部署到自己选择的任意服务器；项目不预设服务器架构。
- 产品名称、模块路径、数据目录和管理页面统一为 **M365 Gateway**。

## 主要能力

- OpenAI 兼容：`/v1/models`、`/v1/chat/completions`、`/v1/responses`
- Anthropic 兼容：`/v1/messages`
- 文本流式响应、SSE 保活、明确的失败终止事件
- 多轮工具调用、`previous_response_id` 续接和工具结果关联
- 会话持久化、上下文预算、长任务超时与工具轮次限制
- 单活动账号模式：确定性顺序切换，不做随机轮换
- 失败账号隔离与冷却；已绑定会话不会无故跨账号漂移
- 每账号可选代理，默认直连；代理配置由用户自行决定
- OAuth PKCE 添加账号、令牌刷新与刷新单飞控制
- API Key 创建、有效期调整、撤销和仅哈希落盘
- 管理员登录限速、强制修改初始密码、安全 Cookie 与安全响应头
- 管理控制台：账号、代理、访问密钥、运行设置、会话缓存和诊断日志
- 非 root、只读根文件系统、最小 Linux capabilities 的 Docker 示例

## 快速开始：Docker

需要 Docker Engine 及 Docker Compose 插件。

```bash
git clone https://github.com/vipamess/Copilot-Bridge-.git
cd Copilot-Bridge-
docker compose up -d --build
```

打开 <http://127.0.0.1:4141>。

- 初始管理员密码：`admin888`
- 首次登录后，控制台会强制改为至少 12 个字符的新密码。
- 在修改完成前，账号、密钥、设置等管理接口保持锁定。
- 强制修改状态会跨服务重启保留，不能通过重启绕过。

查看状态和日志：

```bash
docker compose ps
docker compose logs -f --tail=200 gateway
```

## 快速开始：源码运行

需要 Go 1.27 或更高版本。

Windows PowerShell：

```powershell
git clone https://github.com/vipamess/Copilot-Bridge-.git
Set-Location .\Copilot-Bridge-
.\scripts\run-local.ps1
```

Linux/macOS：

```bash
git clone https://github.com/vipamess/Copilot-Bridge-.git
cd Copilot-Bridge-
chmod +x scripts/run-local.sh
./scripts/run-local.sh
```

脚本只会在当前项目的 `data/` 下创建运行数据，不会写入源码文件。裸机、二进制、Docker、systemd 和通用 HTTPS 反向代理的完整步骤见 [INSTALL.md](INSTALL.md)。

## 首次配置

1. 登录管理控制台并立即修改 `admin888`。
2. 进入“添加账号”，发起 Microsoft OAuth PKCE 授权。
3. 按页面说明登录你有权使用的账号，并提交完整回调地址。
4. 在“账号池”确认账号在线；默认连接方式为直连。
5. 在“访问配置”创建 API Key。完整密钥只显示一次，请立即保存到安全位置。
6. 将客户端 Base URL 设为 `http://127.0.0.1:4141/v1`，API Key 使用刚创建的 `m365_...` 密钥。

## API 示例

模型列表：

```bash
export M365_GATEWAY_API_KEY='paste-your-created-key-here'
curl http://127.0.0.1:4141/v1/models \
  -H "Authorization: Bearer ${M365_GATEWAY_API_KEY}"
```

流式 Chat Completions：

```bash
curl -N http://127.0.0.1:4141/v1/chat/completions \
  -H "Authorization: Bearer ${M365_GATEWAY_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.6-sol",
    "stream": true,
    "messages": [{"role": "user", "content": "请用三点解释这个项目。"}]
  }'
```

Responses：

```bash
curl -N http://127.0.0.1:4141/v1/responses \
  -H "Authorization: Bearer ${M365_GATEWAY_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.6-sol",
    "stream": true,
    "input": "写一个简短的健康检查脚本"
  }'
```

客户端通常只需配置：

```text
Base URL: http://127.0.0.1:4141/v1
API Key:  m365_你的密钥
Model:    gpt-5.6-sol
```

## 账号路由原则

网关采用一个活动账号处理新请求，并按账号列表的固定顺序选择后继账号。它不会为每个请求随机换号。发生可归因于当前账号或其连接路径的错误时，当前账号进入与错误类型相匹配的冷却或隔离状态，再尝试下一个健康账号；全局上游故障不会盲目消耗全部备用账号。已有会话优先保持原账号与原上游会话，以避免上下文串线。

代理不是必需项。新账号默认直连，只有管理员明确保存代理地址后才使用该代理。部署者应自行验证代理的稳定性、所在地、合规性和账号风险。

## 数据位置

Docker 使用命名卷 `m365-gateway-data`，容器内路径为 `/data`。本地脚本使用项目根目录下的 `data/`。可能产生的运行文件包括：

- `accounts.json`：账号标识及 OAuth 令牌，属于高敏感数据
- `sessions.json`：会话与上游对话映射
- `api-keys.json`：API Key 元数据与 SHA-256 哈希，不保存完整密钥
- `admin-password.hash`：管理员密码 bcrypt 哈希
- `settings.json`、`stats.json`、`account-route.json`
- `debug-logs.jsonl`：按日志等级产生的诊断信息

这些路径全部被 `.gitignore` 排除。不要把 `data/`、`.env`、日志、HAR 文件、回调 URL 或截图提交到公开仓库。

## 安全边界

- 默认只监听 `127.0.0.1`。
- 若部署到服务器，建议仍绑定回环地址，由 Nginx/Caddy 提供 HTTPS。
- 不要把 4141 端口无认证暴露到公网。
- `admin888` 是公开的引导密码，不是可长期使用的秘密。
- API Key 只保护本项目的 `/v1/*` 接口，不替代主机防火墙、TLS 和系统更新。
- 账号令牌可代表用户访问上游服务，备份和日志都必须按机密信息保护。

详见 [SECURITY.md](SECURITY.md)。

## 已知限制

- 上游浏览器协议不是稳定的公开 API，可能在没有通知的情况下变化。
- `/v1/realtime` 明确返回 `501 realtime_not_supported`；本版本不提供 OpenAI Realtime 语音 WebSocket。
- 模型是否实际可用取决于账号、租户和上游当时开放的能力。
- 上游不提供完整、统一的真实 token 用量，部分统计为网关估算值。
- 代理、企业网络、DNS、TLS 中间设备和上游限流都可能影响长连接。
- 兼容接口不保证覆盖每个第三方客户端的私有扩展字段。

## 文档

- [完整安装与运维](INSTALL.md)
- [配置变量参考](docs/CONFIGURATION.md)
- [安全策略](SECURITY.md)
- [免责声明](DISCLAIMER.md)
- [公开发行净化审计](PUBLIC_RELEASE_AUDIT.md)
- [第三方版权告知](THIRD_PARTY_NOTICES.md)
- [贡献指南](CONTRIBUTING.md)
- [变更记录](CHANGELOG.md)

## 许可证

本独立版本以 [GNU AGPL-3.0-only](LICENSE) 发布。通过网络向用户提供修改版服务时，请遵守 AGPL 对应源码提供义务。第三方组件的版权与许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
