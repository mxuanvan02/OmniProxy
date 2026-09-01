# OmniProxy

An AI API proxy and routing gateway written in Go. It exposes OpenAI- and Anthropic-compatible endpoints to local CLI tools and routes requests to multiple upstream providers (OpenAI Codex, Google Antigravity, AWS IAM SSO / Builder ID, OpenAI-compatible gateways, AgentRouter, web-search services, and the Gommo AutoAI media API).

OmniProxy is derived from the **SuperKiro** project, extended with model-family grouped catalogs, bi-directional AgentRouter protocol transcoding, account-pool rotation with per-model failure isolation, and a local admin dashboard.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Features

* **Protocol translation:** Serves `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses`, and `/v1/models`.
* **Authentication methods:**
  * **OpenAI Codex OAuth** — browser PKCE flow, automatic token refresh, quota-window tracking.
  * **Google Antigravity OAuth** — browser PKCE flow against Cloud Code Assist, per-account project discovery, and import of credentials an installed Antigravity / Gemini CLI already wrote locally. See the note in [§7](#7-google-antigravity-terms-of-service) before enabling it.
  * **AgentRouter** — converts the agent payload format, maps `agent_thought` stream events to `reasoning_content`, and maintains `X-Agent-Session-ID` across turns.
  * **OpenAI-compatible gateways** — any external endpoint, with model catalog discovery from `/v1/models`.
  * **AWS IAM SSO / Builder ID** — login and background token refresh for CodeWhisperer/Kiro.
  * **Service API keys** — web search via Firecrawl, Tavily, Exa, Jina Reader.
  * **Gommo AutoAI** — long-lived token for the media API behind `api.gommo.net` (also served by the 79AI front end).
* **Media generation:** Image (`/v1/images/generations`), speech (`/v1/audio/speech`), and video (`/v1/videos/generations`, with `/v1/videos/{id}` for a render that outlives the request) are served from providers that expose them. Asynchronous upstream jobs are polled internally, so a client receives a finished result rather than a job id.
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
│        ┌──────────────┬───────────┼───────────┬──────────────┐         │
│        ▼              ▼           ▼           ▼              ▼         │
│  ┌───────────┐ ┌────────────┐ ┌───────────┐ ┌──────────┐ ┌──────────┐  │
│  │   Codex   │ │Antigravity │ │AgentRouter│ │ External │ │  Gommo   │  │
│  │   OAuth   │ │ (Gemini-   │ │ (Protocol │ │ Adapters │ │ (Async   │  │
│  │  (PKCE)   │ │  shaped)   │ │ Transcode)│ │ (OpenAI) │ │  media)  │  │
│  └─────┬─────┘ └─────┬──────┘ └─────┬─────┘ └────┬─────┘ └────┬─────┘  │
└────────┼─────────────┼──────────────┼────────────┼────────────┼────────┘
         │             │              │            │            │
         ▼             ▼              ▼            ▼            ▼
  [OpenAI Codex] [Cloud Code    [AgentRouter]  [External   [api.gommo.net]
                   Assist]                     Providers]
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

`gommoDomain` is part of the Gommo credential rather than an optional setting: the upstream sends it as a body field on every call and rejects a request without it. A Gommo account must not carry the `chat` capability — it generates media and cannot answer a completion, so the chat pool would route a request it can never serve.

Antigravity accounts are written by the login/import flow rather than by hand; `googleProjectId` is discovered per account and should not be copied between accounts.

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

### Media generation

These routes need an account that advertises the matching capability (see the
Gommo entry in [§4](#4-configuration-dataconfigjson)).

```bash
# Image — answers with a URL per image
curl http://127.0.0.1:8080/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"a paper boat on still water","size":"1024x1024","n":1}'

# Speech — answers with audio bytes, as the OpenAI route does
curl http://127.0.0.1:8080/v1/audio/speech \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"input":"Xin chào","voice":"<voice-id>"}' \
  --output speech.mp3

# Video — the render is polled internally; `id` lets you retrieve a slow one later
curl http://127.0.0.1:8080/v1/videos/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"a paper boat drifting downstream","ratio":"16_9"}'

curl http://127.0.0.1:8080/v1/videos/<id> \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY"
```

---

## 6. Testing

```bash
go test -count=1 ./...
```

---

## 7. Google Antigravity Terms of Service

Google's Antigravity terms permit access only through Google's own client. An
account used through this proxy can be disabled by Google at any time, and that
has happened to people in practice. Treat it as an expected outcome of enabling
the provider, not an edge case.

The OAuth client is not shipped here. Antigravity's desktop client ID and secret
belong to Google, so the login reads them from `ANTIGRAVITY_CLIENT_ID` and
`ANTIGRAVITY_CLIENT_SECRET` (or the `antigravityClientId` /
`antigravityClientSecret` settings) and fails with a clear message when neither
is set. Take the values from your own installed IDE. Importing credentials that
Antigravity or the Gemini CLI already wrote locally needs neither.

What OmniProxy does about it:

* It sends the protocol fields the Cloud Code Assist API requires — the OAuth
  token, the `Client-Metadata` descriptor, and the account's own project — and
  nothing beyond them.
* It does not rotate client fingerprints, invent platform or api-client values,
  or send a hardcoded project id belonging to someone else. Several third-party
  clients do the last one; requests then bill against a project the caller has no
  claim to, which is the pattern enforcement looks for.
* When Google does disable an account, `classifyAntigravityFailure` recognises
  the response and marks the account `BANNED`, so the pool stops selecting it
  instead of retrying a dead credential on every request.

No configuration makes this compliant with those terms. Decide whether to use it
with that in mind; the other providers here are unaffected either way.

---

## 8. License

Released under the [MIT](LICENSE) License. Derived from the SuperKiro project.
