# 配置参考

配置优先级为：显式环境变量 > 管理页面保存的设置 > 程序默认值。被环境变量覆盖的字段会在管理页面标记，修改后通常需要调整环境变量并重启才会生效。

## 基础配置

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `M365_LISTEN` | `127.0.0.1:4141` | HTTP 监听地址；容器内部使用 `0.0.0.0:4141` |
| `M365_ADMIN_PASSWORD` | 无；发行脚本设为 `admin888` | 首次启动引导密码，写入 bcrypt 后由持久化哈希接管 |
| `M365_ADMIN_PASSWORD_HASH_FILE` | 用户配置目录 | 可写的管理员密码哈希文件 |
| `M365_ADMIN_BOOTSTRAP_PASSWORD_FILE` | 空 | 从只读文件读取引导密码，优先于密码环境变量 |
| `M365_COOKIE_SECURE` | `false` | HTTPS 部署应设为 `true` |
| `M365_LOG_LEVEL` | `warn` | `silent`、`error`、`warn`、`info`、`debug` |
| `M365_MAX_REQUEST_BODY_BYTES` | `8388608` | 请求体大小上限 |

兼容变量 `M365_ADMIN_PASSWORD_FILE` 和 `M365_TOKEN_FILE` 仅用于迁移旧配置，新部署应使用上表和下表中的主变量。

## 数据路径

| 变量 | Docker 默认路径 | 内容 |
|---|---|---|
| `M365_TOKEN_CACHE` | `/data/accounts.json` | 账号、OAuth token 和每账号代理 |
| `M365_SESSION_CACHE` | `/data/sessions.json` | 客户端会话、response alias 与上游对话映射 |
| `M365_API_KEYS` | `/data/api-keys.json` | API Key 哈希、前缀、有效期和撤销状态 |
| `M365_SETTINGS_FILE` | `/data/settings.json` | 管理页保存的运行设置 |
| `M365_DEBUG_LOG` | `/data/debug-logs.jsonl` | 结构化诊断日志 |
| `M365_CONFIG` | 空 | 兼容账号配置路径 |

这些文件不得指向同一路径。Docker named volume 和本地 `scripts/run-local.*` 已提供安全的独立默认值。

## 长任务、上下文与工具

| 变量 | 默认值 | 约束与说明 |
|---|---:|---|
| `M365_CHAT_TIMEOUT_SECONDS` | `600` | 单次聊天最长 5–3600 秒；反向代理超时应更长 |
| `M365_IMAGE_TIMEOUT_SECONDS` | `150` | 图片请求最长 5–3600 秒 |
| `M365_MAX_TOOL_ROUNDS` | `64` | 每个任务最多 1–512 个工具轮次 |
| `M365_MAX_TOOL_CALLS_PER_TURN` | `4` | 每轮最多 1–64 个工具调用；客户端禁止并行时强制为 1 |
| `M365_CONTEXT_WINDOW` | `128000` | 未知模型使用的兼容上下文窗口 |
| `M365_MAX_OUTPUT_TOKENS` | `16384` | 未知模型使用的最大输出，必须小于上下文窗口 |
| `M365_INPUT_BUDGET_TOKENS` | 模型相关 | 全请求输入预算；过小的无效值会回退到安全默认值 |
| `M365_CONTINUING_HISTORY_SHARE` | `35` | 续接会话可用于旧历史的百分比，范围 0–100 |

不要盲目把工具轮次和超时调到最大。较大值会增加上游配额消耗、客户端等待时间和重复工具失败的影响。网关会保留当前任务、开发者指令、最近承诺和工具调用/结果原子单元；超过预算时会明确拒绝无法安全保留的当前输入，而不是静默丢失关键指令。

## OAuth

| 变量 | 说明 |
|---|---|
| `M365_CLIENT_ID` | OAuth 客户端 ID；通常使用内置兼容值，除非你有自己的授权配置 |
| `M365_AUTHORITY` | OAuth authority |
| `M365_REDIRECT_URI` | OAuth 回调 URI |
| `M365_SCOPE` | OAuth scope |
| `M365_AUTHORIZE_ENDPOINT` | 高级：覆盖授权端点 |
| `M365_TOKEN_ENDPOINT` | 高级：覆盖 token 端点 |
| `M365_DEVICE_ENDPOINT` | 高级：覆盖 device-code 端点 |

覆盖 OAuth 端点可能把凭据发送到错误服务。除非在受控环境中验证过端点归属和 TLS，否则不要修改。

## 代理与证书

代理在管理页按账号配置；新账号默认直连。支持的具体 URL 形式由代理实现决定，保存前应使用页面测试功能验证。

| 变量 | 说明 |
|---|---|
| `M365_PROXY_CA_FILE` | 为受控企业代理添加额外 CA 证书；不会替换系统 CA |

不要关闭 TLS 校验。额外 CA 文件必须来自你信任的管理员，并以只读方式挂载。

## 高风险诊断开关

以下变量默认关闭：

| 变量 | 风险 |
|---|---|
| `M365_INCLUDE_UPSTREAM_EVENTS=true` | 在兼容元数据中加入上游原始事件，可能泄露内部字段 |
| `M365_TRACE_PROTOCOL=1` | 输出协议帧，可能包含对话和标识符 |
| `M365_ROUTER_DEBUG=1` | 输出工具路由提示和结果片段 |

只在隔离环境、短时间、使用测试账号时开启。排障结束后关闭并安全删除日志。

`M365_STICKY_ACCOUNT` 是旧兼容变量；当前版本始终采用单活动账号和确定性切换，该变量不能重新启用随机轮换。

## 推荐配置档

### 本机开发

```dotenv
M365_LISTEN=127.0.0.1:4141
M365_COOKIE_SECURE=false
M365_LOG_LEVEL=info
M365_CHAT_TIMEOUT_SECONDS=600
```

### HTTPS 服务器

```dotenv
M365_LISTEN=127.0.0.1:4141
M365_COOKIE_SECURE=true
M365_LOG_LEVEL=warn
M365_CHAT_TIMEOUT_SECONDS=600
```

服务器仍应配合 TLS 反向代理、防火墙、非 root 用户、加密备份和受限管理网络使用。
