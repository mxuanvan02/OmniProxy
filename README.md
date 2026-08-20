# OmniProxy

An extensible AI API proxy and routing gateway written in Go. It bridges developer tools (Hermes Agent, OpenClaw, K-Dense BYOK, Claude CLI, Cursor, Continue) with diverse Large Language Model providers (OpenAI Codex, AWS IAM SSO/Builder ID, External OpenAI-compatible Providers, AgentRouter, and Web Search engines).

OmniProxy is built upon and evolved from the **SuperKiro** project, expanding it with model-family grouped catalogs, bi-directional AgentRouter protocol transcoding, tiered account rotation, and local administration.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Key Features

* **Multi-Protocol Translation:** Exposes standard `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses`, and `/v1/models` endpoints to all clients.
* **Multi-Provider Authentication Matrix:**
  * **OpenAI Codex OAuth:** Web PKCE authentication flow with automatic token refresh and quota window tracking.
  * **AgentRouter Protocol Transcoder:** Decodes custom agent payload structures, translates `agent_thought` events into standard `reasoning_content` streams, and manages persistent `X-Agent-Session-ID` sessions.
  * **External OpenAI-compatible Gateways:** Connects external API gateways with dynamic model catalog discovery.
  * **AWS IAM SSO & Builder ID:** Native login and background token refreshing for CodeWhisperer/Kiro.
  * **Service API Keys:** Integrated web search via Firecrawl, Tavily, Exa, and Jina Reader.
* **Model Family Routing & Unified Catalog:** Automatically organizes 500+ model IDs into 9 families (`gpt`, `claude`, `qwen`, `deepseek`, `glm`, `grok`, `llama`, `kimi`, `minimax`) with accurate context and output token limits.
* **Account Pool & Failover Strategies:**
  * Routing algorithms: Weighted Round-Robin, Cost-Optimized, and Reset-Aware.
  * **Per-model locking:** Isolates failures at the model level per account, avoiding broad account disqualification on transient model rate-limits.
  * **Sticky session prompt caching:** Keeps sequential conversation turns on the same upstream account to maximize provider prompt cache reuse.
* **Local Web Dashboard & Monitoring:** Embedded local UI in 3 languages (EN, VI, ZH), daily token/credit usage tracking, API key management, and fallback combo configuration.

---

## 2. Architecture

```
[Clients: Hermes Agent, OpenClaw, K-Dense, Claude CLI, Cursor]
                                │
                                ▼  (/v1/chat/completions, /v1/messages, /v1/models)
┌────────────────────────────────────────────────────────────────────────┐
│                              OmniProxy                                 │
│                                                                        │
│  ┌──────────────────────┐  ┌─────────────────┐  ┌──────────────────┐  │
│  │   Inbound Handler    │  │   Model Catalog │  │  API Key & Quota │  │
│  │ (OpenAI / Anthropic) │  │  (Family Groups)│  │     Manager      │  │
│  └──────────┬───────────┘  └────────┬────────┘  └─────────┬────────┘  │
│             │                       │                     │           │
│             ▼                       ▼                     ▼           │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                       Account Routing Pool                       │  │
│  │  - Round-Robin / Cost-Optimized / Reset-Aware                    │  │
│  │  - Per-Model Cooldown & Error Isolation                          │  │
│  │  - Sticky Prompt Cache Router                                    │  │
│  └──────────────────────────────────┬───────────────────────────────┘  │
│                                     │                                  │
│             ┌───────────────────────┼───────────────────────┐          │
│             ▼                       ▼                       ▼          │
│  ┌─────────────────────┐ ┌─────────────────────┐ ┌──────────────────┐  │
│  │  Codex OAuth Client │ │ AgentRouter Adapter │ │ External Adapters│  │
│  │  (PKCE / SSE Stream)│ │ (Protocol Transcode)│ │ (OpenAI / Search)│  │
│  └──────────┬──────────┘ └──────────┬──────────┘ └──────────┬───────┘  │
└─────────────┼───────────────────────┼───────────────────────┼──────────┘
              │                       │                       │
              ▼                       ▼                       ▼
      [OpenAI Codex]            [AgentRouter]       [External Providers]
```

---

## 3. Installation & Getting Started

### Prerequisites
* Go 1.25+ (or Docker).

### Build from Source
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

The server listens on `http://127.0.0.1:8080` (or the port defined in `data/config.json`). The web dashboard is accessible at `http://127.0.0.1:8080/web/`.

### Run with Docker
```bash
docker compose up -d
```

### Run on macOS (launchd)
```bash
cp com.van.omniproxy.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.van.omniproxy.plist
```

---

## 4. Configuration Example (`data/config.json`)

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

## 5. Client Integration

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
In your `.env` file:
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

## 6. Testing

Run the full test suite:
```bash
go test -count=1 ./...
```

---

## 7. License

Released under the [MIT](LICENSE) License. Built upon the foundation of SuperKiro.
