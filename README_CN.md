# OmniProxy

使用 Go 编写的 AI API 代理与路由网关。它向本地 CLI 工具暴露 OpenAI 与 Anthropic 兼容的接口，并将请求路由到多个上游提供商（OpenAI Codex、Google Antigravity、AWS IAM SSO / Builder ID、OpenAI 兼容网关、AgentRouter、网页搜索服务，以及 Gommo AutoAI 媒体 API）。

OmniProxy 派生自 **SuperKiro** 项目，并在其基础上扩展了按模型家族分组的目录、AgentRouter 协议双向转换、带按模型故障隔离的账户池轮换，以及本地管理面板。

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. 功能

* **协议转换：** 提供 `/v1/chat/completions`、`/v1/messages`（Anthropic）、`/v1/responses` 与 `/v1/models`。
* **认证方式：**
  * **OpenAI Codex OAuth** — 浏览器 PKCE 流程、令牌自动刷新、额度窗口追踪。
  * **Google Antigravity OAuth** — 面向 Cloud Code Assist 的浏览器 PKCE 流程、按账户发现 project，并可导入本机已安装的 Antigravity / Gemini CLI 写入的凭据。启用前请先阅读 [§7](#7-google-antigravity-服务条款) 的说明。
  * **AgentRouter** — 转换 agent 载荷格式，将 `agent_thought` 流事件映射为 `reasoning_content`，并在多轮之间维持 `X-Agent-Session-ID`。
  * **OpenAI 兼容网关** — 任意外部端点，通过 `/v1/models` 发现模型目录。
  * **AWS IAM SSO / Builder ID** — CodeWhisperer/Kiro 的登录与后台令牌刷新。
  * **服务 API Key** — 通过 Firecrawl、Tavily、Exa、Jina Reader 进行网页搜索。
  * **Gommo AutoAI** — 用于 `api.gommo.net` 背后媒体 API 的长期令牌（79AI 前端使用的也是同一后端）。
* **媒体生成：** 图像（`/v1/images/generations`）、语音（`/v1/audio/speech`）与视频（`/v1/videos/generations`，渲染时间超出请求时可用 `/v1/videos/{id}` 取回）由具备相应能力的提供商承担。上游的异步任务在内部轮询完成，因此客户端拿到的是最终结果而不是一个任务 ID。
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
│        ┌──────────────┬───────────┼───────────┬──────────────┐         │
│        ▼              ▼           ▼           ▼              ▼         │
│  ┌───────────┐ ┌────────────┐ ┌───────────┐ ┌──────────┐ ┌──────────┐  │
│  │   Codex   │ │Antigravity │ │AgentRouter│ │ 外部兼容 │ │  Gommo   │  │
│  │   OAuth   │ │ (Gemini    │ │  (协议    │ │ 提供商   │ │ (异步    │  │
│  │  (PKCE)   │ │  形态)     │ │  转换)    │ │ (OpenAI) │ │  媒体)   │  │
│  └─────┬─────┘ └─────┬──────┘ └─────┬─────┘ └────┬─────┘ └────┬─────┘  │
└────────┼─────────────┼──────────────┼────────────┼────────────┼────────┘
         │             │              │            │            │
         ▼             ▼              ▼            ▼            ▼
  [OpenAI Codex] [Cloud Code    [AgentRouter]  [第三方       [api.gommo.net]
                   Assist]                     提供商]
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
    },
    {
      "id": "00000000-0000-0000-0000-000000000003",
      "nickname": "Gommo Media",
      "authMethod": "gommo",
      "provider": "Gommo AutoAI",
      "accessToken": "<gommo-access-token>",
      "gommoDomain": "79ai.net",
      "gommoProjectId": "default",
      "imageModel": "<image-model-id>",
      "gommoTtsModel": "eleven_flash_v2_5",
      "gommoVoiceId": "<voice-id>",
      "capabilities": ["image", "video", "audio-tts"],
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

`gommoDomain` 属于 Gommo 凭据本身，而非可选设置：上游要求每次调用都在请求体中携带它，缺失则直接拒绝。Gommo 账户不能带 `chat` 能力 — 它只生成媒体、无法回答对话请求，否则 chat 池会把永远无法完成的请求路由给它。

Antigravity 账户由登录/导入流程写入，不建议手写；`googleProjectId` 会按账户各自发现，切勿在账户之间复制。

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

### 媒体生成

这些路由需要一个具备相应能力的账户（参见 [§4](#4-配置-dataconfigjson) 中的 Gommo 示例）。

```bash
# 图像 — 每张图返回一个 URL
curl http://127.0.0.1:8080/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"a paper boat on still water","size":"1024x1024","n":1}'

# 语音 — 与 OpenAI 路由一致，直接返回音频字节
curl http://127.0.0.1:8080/v1/audio/speech \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"input":"你好","voice":"<voice-id>"}' \
  --output speech.mp3

# 视频 — 渲染在内部轮询；`id` 用于稍后取回耗时较长的任务
curl http://127.0.0.1:8080/v1/videos/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"a paper boat drifting downstream","ratio":"16_9"}'

curl http://127.0.0.1:8080/v1/videos/<id> \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY"
```

---

## 6. 测试

```bash
go test -count=1 ./...
```

---

## 7. Google Antigravity 服务条款

Google 的 Antigravity 条款只允许通过 Google 自家的客户端访问。通过本代理使用的
账号随时可能被 Google 停用，实际上已有人遇到这种情况。请把它当作启用该提供商的
预期结果，而不是边缘案例。

本仓库不附带 OAuth 客户端。Antigravity 桌面客户端的 ID 与 secret 属于 Google，因此
登录流程从 `ANTIGRAVITY_CLIENT_ID` 与 `ANTIGRAVITY_CLIENT_SECRET`（或
`antigravityClientId` / `antigravityClientSecret` 设置项）读取；两者都未设置时会以
明确的错误信息失败。请从你自己安装的 IDE 中取值。导入 Antigravity 或 Gemini CLI
已写入本机的凭据则不需要这两个值。

OmniProxy 在这方面的处理：

* 只发送 Cloud Code Assist API 要求的协议字段 —— OAuth 令牌、`Client-Metadata`
  描述符，以及该账号自己的 project —— 除此之外不多发任何东西。
* 不轮换客户端指纹、不伪造 platform 或 api-client 值，也不发送属于他人的硬编码
  project id。若干第三方客户端正是这么做的：请求随后计费到调用方本无权使用的
  project 上，而这恰恰是执法方所排查的模式。
* 当 Google 确实停用某个账号时，`classifyAntigravityFailure` 会识别该响应并把账号
  标记为 `BANNED`，账户池随即停止选择它，而不是每次请求都重试一个已失效的凭据。

没有任何配置能让这种用法符合上述条款。请在了解这一点的前提下决定是否使用；本项目
的其他提供商不受影响。

---

## 8. 许可证

基于 [MIT](LICENSE) 许可证发布。派生自 SuperKiro 项目。
