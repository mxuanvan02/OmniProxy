# OmniProxy

An AI API proxy and routing gateway written in Go. It exposes OpenAI- and Anthropic-compatible endpoints to local CLI tools and routes requests to multiple upstream providers (OpenAI Codex, AWS IAM SSO / Builder ID, OpenAI-compatible gateways, AgentRouter, and web-search services).

OmniProxy is derived from the **SuperKiro** project, extended with model-family grouped catalogs, bi-directional AgentRouter protocol transcoding, account-pool rotation with per-model failure isolation, and a local admin dashboard.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Features

* **Protocol translation:** Serves `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses`, and `/v1/models`.
* **Authentication methods:**
  * **OpenAI Codex OAuth** — browser PKCE flow, automatic token refresh, quota-window tracking.
  * **AgentRouter** — converts the agent payload format, maps `agent_thought` stream events to `reasoning_content`, and maintains `X-Agent-Session-ID` across turns.
  * **OpenAI-compatible gateways** — any external endpoint, with model catalog discovery from `/v1/models`.
  * **AWS IAM SSO / Builder ID** — login and background token refresh for CodeWhisperer/Kiro.
  * **Service API keys** — web search via Firecrawl, Tavily, Exa, Jina Reader.
* **Model family catalog:** Groups discovered model IDs into families (`gpt`, `claude`, `qwen`, `deepseek`, `glm`, `grok`, `llama`, `kimi`, `minimax`) with context and output token limits.
* **Account pool:**
  * Selection strategies: weighted round-robin, cost-optimized, reset-aware.
  * Per-model cooldown, so a rate-limited model does not disqualify the whole account.
  * Sticky routing per conversation to reuse the upstream prompt cache.
* **Local dashboard:** Embedded web UI (EN / VI / ZH), daily token and credit usage, API key management, fallback combos.

---

## 2. Architecture

```
[Clients: Claude CLI, Codex CLI, any OpenAI-compatible client]
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

## 3. Installation

### Prerequisites
* Go 1.25+ (or Docker).

### Build from source
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

The server listens on `http://127.0.0.1:8080` (or the port in `data/config.json`). The dashboard is at `http://127.0.0.1:8080/admin`.

### Run with Docker
```bash
docker compose up -d
```

> The default configuration binds without an API key requirement. Set `requireApiKey: true` and change `password` before exposing the port beyond loopback.

---

## 4. Configuration (`data/config.json`)

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

Accounts can also be added from the dashboard (`/admin` → Add Account), which handles the OAuth and import flows.

---

## 5. Client Integration

### Claude CLI

Point the Anthropic base URL at OmniProxy:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=<your-omniproxy-api-key>   # any value when requireApiKey is false

claude --model claude-opus-5 -p "Hello"
```

Or persist it in `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"
  }
}
```

### Codex CLI

Add OmniProxy as an OpenAI-compatible provider in `~/.codex/config.toml`:

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

### Direct API calls

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

# Available models
curl http://127.0.0.1:8080/v1/models
```

---

## 6. Testing

```bash
go test -count=1 ./...
```

---

## 7. License

Released under the [MIT](LICENSE) License. Derived from the SuperKiro project.
