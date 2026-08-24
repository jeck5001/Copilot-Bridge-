# Changelog

## 1.0.0 — 2026-08-24

- 初始化独立品牌 M365 Gateway。
- 创建不含账号、密钥、代理、域名、IP、日志和服务器拓扑的空白公开版本。
- 提供 OpenAI Chat Completions、Responses 和 Anthropic Messages 兼容接口。
- 提供流式保活、失败终止、长任务超时和上下文预算治理。
- 提供多轮工具调用、`previous_response_id` 续接和工具状态校验。
- 使用单活动账号、固定顺序故障切换、账号隔离和分类冷却。
- 提供 OAuth PKCE、API Key 生命周期、账号代理和诊断管理页面。
- 管理员初始密码固定为 `admin888` 并强制修改；强制状态跨重启保持。
- 增加通用本机、Docker、服务器、systemd 和 HTTPS 安装说明。
- 增加安全策略、免责声明、第三方告知和公开发行净化审计。
