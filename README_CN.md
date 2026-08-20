# OmniProxy

基于 Go 语言实现的 LLM API 代理网关与路由系统。用于连接各类软件开发与 Agent 客户端（Hermes Agent、OpenClaw、K-Dense BYOK、Claude CLI、Cursor、Continue 等）与多个大模型服务提供商（OpenAI Codex、AWS IAM SSO/Builder ID、标准 OpenAI 兼容网关、AgentRouter 以及 Web 搜索服务）。

OmniProxy 基于 **SuperKiro** 开源项目进行重构与功能扩展，新增了 AgentRouter 协议双向转换、模型家族分类管理、多层级账户轮换与故障隔离机制以及本地管理面板。

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. 核心功能

* **多协议接口转换：** 提供标准 `/v1/chat/completions`、`/v1/messages` (Anthropic)、`/v1/responses` 与 `/v1/models` 端点。
* **多鉴权与提供商矩阵：**
  * **OpenAI Codex OAuth：** 基于浏览器的 PKCE 登录流程，支持令牌自动静默刷新与额度重置周期追踪。
  * **AgentRouter 协议转换：** 支持专用任务载荷格式，将 `agent_thought` 流式事件实时解析转换为标准 OpenAI `reasoning_content`，维护 `X-Agent-Session-ID` 会话状态。
  * **外部 OpenAI 兼容端点：** 支持接入各类 OpenAI 兼容第三方接口并自动发现模型列表。
  * **AWS IAM SSO & Builder ID：** 支持 CodeWhisperer/Kiro 的 SSO 登录与令牌刷新。
  * **Service API Keys：** 内置集成 Firecrawl、Tavily、Exa 及 Jina Reader 网络搜索服务。
* **模型家族分类与聚合目录：** 将 500+ 模型标识按 9 大家族（`gpt`、`claude`、`qwen`、`deepseek`、`glm`、`grok`、`llama`、`kimi`、`minimax`）归类，并提供精确的上下文与输出 Token 限制。
* **账户池调度与容灾隔离：**
  * 支持加权轮询（Weighted Round-Robin）、成本优化（Cost-Optimized）以及重置周期感知（Reset-Aware）调度策略。
  * **按模型粒度故障锁定（Per-model lock）：** 发生限流或异常时仅在账户内隔离对应模型，避免单一模型错误导致整个账户被剔除。
  * **会话级 Prompt 缓存粘性（Sticky Prompt Cache）：** 保持同一对话上下文命中相同上游账户，提升上游 Prompt Cache 命中率。
* **本地管理面板与监控：** 提供三语（英文、中文、越南语）Web UI、每日 Token 与额度使用统计、API Key 额度管控及模型降级组合（Combos）配置。

---

## 2. 架构概览

```
[客户端: Hermes Agent, OpenClaw, K-Dense, Claude CLI, Cursor]
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
│  │  (PKCE / SSE 流转换)│ │ (任务载荷与思考流)  │ │ (OpenAI / 搜索)  │  │
│  └──────────┬──────────┘ └──────────┬──────────┘ └──────────┬───────┘  │
└─────────────┼───────────────────────┼───────────────────────┼──────────┘
              │                       │                       │
              ▼                       ▼                       ▼
      [OpenAI Codex]            [AgentRouter]            [第三方提供商]
```

---

## 3. 安装与运行

### 环境要求
* Go 1.25 或更高版本（或使用 Docker）。

### 源码编译运行
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

服务默认监听 `http://127.0.0.1:8080`（或在 `data/config.json` 中指定的端口）。管理控制台地址为 `http://127.0.0.1:8080/web/`。

### 使用 Docker 部署
```bash
docker compose up -d
```

---

## 4. 配置文件示例 (`data/config.json`)

```json
{
  "host": "0.0.0.0",
  "port": 8080,
  "password": "your-admin-password",
  "requireApiKey": false,
  "accounts": [
    {
      "id": "6d45de3c-84bc-403d-840b-78deb9bc43b9",
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
      "id": "5f369898-a56b-4072-8cbf-c4166553044e",
      "nickname": "DeepSeek External",
      "authMethod": "external_openai",
      "provider": "External OpenAI",
      "baseUrl": "https://api.deepseek.com",
      "accessToken": "sk-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

---

## 5. 客户端集成

### Hermes Agent / OpenClaw
```yaml
providers:
  omniproxy:
    base_url: "http://127.0.0.1:8080/v1"
    api_key: "***"
    models:
      - gpt-5.6-sol
      - claude-opus-5
      - deepseek-v4-pro
      - qwen3.8-max
```

### K-Dense BYOK
在 `.env` 中配置：
```env
OMNIPROXY_BASE_URL=http://127.0.0.1:8080/v1
DEFAULT_MODEL_PROVIDER=omniproxy
```

### Claude CLI (`~/.claude/settings.json`)
```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"
  }
}
```

---

## 6. 自动化测试

运行全套单元与集成测试：
```bash
go test -count=1 ./...
```

---

## 7. 开源协议

本项目采用 [MIT](LICENSE) 开源许可证。基于 SuperKiro 项目构建与扩展。
