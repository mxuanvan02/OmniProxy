# OmniProxy

使用 Go 编写的 AI API 代理与路由网关。它向本地 CLI 工具暴露 OpenAI 与 Anthropic 兼容的接口，并将请求路由到多个上游提供商（OpenAI Codex、AWS IAM SSO / Builder ID、OpenAI 兼容网关、AgentRouter 以及网页搜索服务）。

OmniProxy 派生自 **SuperKiro** 项目，并在其基础上扩展了按模型家族分组的目录、AgentRouter 协议双向转换、带按模型故障隔离的账户池轮换，以及本地管理面板。

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. 功能

* **协议转换：** 提供 `/v1/chat/completions`、`/v1/messages`（Anthropic）、`/v1/responses` 与 `/v1/models`。
* **认证方式：**
  * **OpenAI Codex OAuth** — 浏览器 PKCE 流程、令牌自动刷新、额度窗口追踪。
  * **AgentRouter** — 转换 agent 载荷格式，将 `agent_thought` 流事件映射为 `reasoning_content`，并在多轮之间维持 `X-Agent-Session-ID`。
  * **OpenAI 兼容网关** — 任意外部端点，通过 `/v1/models` 发现模型目录。
  * **AWS IAM SSO / Builder ID** — CodeWhisperer/Kiro 的登录与后台令牌刷新。
  * **服务 API Key** — 通过 Firecrawl、Tavily、Exa、Jina Reader 进行网页搜索。
* **模型家族目录：** 将发现的模型 ID 按家族分组（`gpt`、`claude`、`qwen`、`deepseek`、`glm`、`grok`、`llama`、`kimi`、`minimax`），并附带上下文与输出 token 限制。
* **账户池：**
  * 选择策略：加权轮询、成本优先、重置感知。
  * 按模型冷却，被限流的模型不会导致整个账户被剔除。
  * 按会话粘性路由，以复用上游 prompt 缓存。
* **本地面板：** 内置 Web UI（EN / VI / ZH）、每日 token 与额度用量、API Key 管理、降级组合配置。

---

## 2. 架构

```
[客户端: Claude CLI, Codex CLI, 任意 OpenAI 兼容客户端]
                                │
                                ▼  (/v1/chat/completions, /v1/messages, /v1/models)
┌────────────────────────────────────────────────────────────────────────┐
│                              OmniProxy                                 │
│                                                                        │
│  ┌──────────────────────┐  ┌─────────────────┐  ┌──────────────────┐  │
│  │     入站请求处理器   │  │   模型家族目录  │  │ API Key 与限额   │  │
│  │ (OpenAI / Anthropic) │  │   (按家族分组)  │  │     管理系统     │  │
│  └──────────┬───────────┘  └────────┬────────┘  └─────────┬────────┘  │
│             │                       │                     │           │
│             ▼                       ▼                     ▼           │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                        账户池路由调度引擎                        │  │
│  │  - 加权轮询 / 成本优先 / 重置感知策略                            │  │
│  │  - 按模型粒度的错误冷却与隔离机制                                │  │
│  │  - 会话粘性路由以复用上游 Prompt Cache                           │  │
│  └──────────────────────────────────┬───────────────────────────────┘  │
│                                     │                                  │
│             ┌───────────────────────┼───────────────────────┐          │
│             ▼                       ▼                       ▼          │
│  ┌─────────────────────┐ ┌─────────────────────┐ ┌──────────────────┐  │
│  │  Codex OAuth 客户端 │ │ AgentRouter 转换器  │ │ 外部兼容提供商   │  │
│  │  (PKCE / SSE 流转换)│ │ (载荷与思考流转换)  │ │ (OpenAI / 搜索)  │  │
│  └──────────┬──────────┘ └──────────┬──────────┘ └──────────┬───────┘  │
└─────────────┼───────────────────────┼───────────────────────┼──────────┘
              │                       │                       │
              ▼                       ▼                       ▼
      [OpenAI Codex]            [AgentRouter]            [第三方提供商]
```

---

## 3. 安装

### 环境要求
* Go 1.25 或更高版本（或使用 Docker）。

### 源码编译
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

服务监听 `http://127.0.0.1:8080`（或 `data/config.json` 中配置的端口）。管理面板位于 `http://127.0.0.1:8080/admin`。

### 使用 Docker 部署
```bash
docker compose up -d
```

> 默认配置不强制要求 API Key。在将端口暴露到回环地址之外前，请设置 `requireApiKey: true` 并修改 `password`。

---

## 4. 配置 (`data/config.json`)

```json
{
  "host": "127.0.0.1",
  "port": 8080,
  "password": "your-admin-password",
  "requireApiKey": false,
  "accounts": [
    {
      "id": "00000000-0000-0000-0000-000000000001",
      "nickname": "AgentRouter Primary",
      "authMethod": "agentrouter",
      "provider": "AgentRouter",
      "baseUrl": "https://agentrouter.org",
      "accessToken": "sk-ar-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    },
    {
      "id": "00000000-0000-0000-0000-000000000002",
      "nickname": "External Provider",
      "authMethod": "external_openai",
      "provider": "External OpenAI",
      "baseUrl": "https://api.example.com",
      "accessToken": "sk-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

也可以在管理面板（`/admin` → Add Account）中添加账户，面板会处理 OAuth 与导入流程。

---

## 5. 客户端集成

### Claude CLI

将 Anthropic base URL 指向 OmniProxy：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=<your-omniproxy-api-key>   # requireApiKey 为 false 时可填任意值

claude --model claude-opus-5 -p "Hello"
```

也可以写入 `~/.claude/settings.json` 持久化：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"
  }
}
```

### Codex CLI

在 `~/.codex/config.toml` 中将 OmniProxy 添加为 OpenAI 兼容提供商：

```toml
model = "gpt-5.6-sol"
model_provider = "omniproxy"

[model_providers.omniproxy]
name = "OmniProxy"
base_url = "http://127.0.0.1:8080/v1"
env_key = "OMNIPROXY_API_KEY"
```

```bash
export OMNIPROXY_API_KEY=<your-omniproxy-api-key>
codex exec "Hello"
```

### 直接调用 API

```bash
# Anthropic Messages
curl http://127.0.0.1:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: $OMNIPROXY_API_KEY" \
  -d '{"model":"claude-opus-5","max_tokens":128,"messages":[{"role":"user","content":"Hello"}]}'

# OpenAI Chat Completions
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"Hello"}]}'

# 可用模型
curl http://127.0.0.1:8080/v1/models
```

---

## 6. 测试

```bash
go test -count=1 ./...
```

---

## 7. 许可证

基于 [MIT](LICENSE) 许可证发布。派生自 SuperKiro 项目。
