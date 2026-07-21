<p align="center">
  <a href="https://github.com/mxuanvan02/OmniProxy">
    <picture>
      <img src="web/icon.svg" alt="OmniProxy" style="width: 25%;">
    </picture>
  </a>
</p>

# OmniProxy

<div align="center">
  <a href="https://go.dev/">
    <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go Version">
  </a>
  <a href="https://www.docker.com/">
    <img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker" alt="Docker">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License">
  </a>
  <a href="https://github.com/mxuanvan02/OmniProxy/releases">
    <img src="https://img.shields.io/github/v/release/mxuanvan02/OmniProxy?display_name=tag&sort=semver" alt="Release">
  </a>
  <a href="https://github.com/mxuanvan02/OmniProxy/stargazers">
    <img src="https://img.shields.io/github/stars/mxuanvan02/OmniProxy" alt="Stars">
  </a>
</div>

<div align="center">
  <p>Convert Kiro / Codex accounts into OpenAI &amp; Anthropic compatible API services.</p>
</div>

<div align="center">
  <a href="README.md">English</a> | <a href="README_CN.md">中文</a> | <a href="README_VN.md">Tiếng Việt</a>
</div>

<div align="center">
  <p>If this project helps you, a Star would mean a lot.</p>
</div>

<p align="center">
  <a href="https://github.com/mxuanvan02/OmniProxy">
    <picture>
      <img src="resources/webui.jpg" alt="OmniProxy" style="width: 75%;">
    </picture>
  </a>
</p>

## Features

### Core API

- **API compatibility** — Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` & `/v1/responses`, streaming SSE
- **12 auth methods** — AWS Builder ID, IAM Identity Center, SSO Token, Social Login, Kiro CLI import, Kiro SSO 3-step browser login, AWS SSO Cache, Kiro Local Cache, Credentials JSON, Kiro Web Cookie, API Key (ksk_), Refresh Token
- **Auto token refresh** — credentials stay valid without manual intervention
- **Prompt filters** — replace Claude Code CLI system prompts with compact backend version, strip env noise and boundary markers; custom regex rules (admin panel)
- **Endpoint config** — auto-select, Kiro, CodeWhisperer, or Amazon-Q endpoint with optional fallback
- **Per-account outbound proxy** — global or account-level SOCKS5 / HTTP proxy
- **Usage tracking** — per-account credits, tokens, request counts, overage alerts
- **Thinking mode** — configurable suffix trigger, output format (reasoning_content / thinking / think)
- **Web admin panel** — manage accounts, settings, i18n (EN / CN / VN)

### Multi-Account Pool

- **Round-robin load balancing** — weighted distribution across accounts
- **Endpoint failover** — automatic switch on errors, combo fallback chains
- **Provider-aware routing** — Claude models route to external OpenAI-compatible accounts, GPT models route to Codex accounts
- **Per-model cooldown** — quota/auth failures lock only the affected model, not the whole account

### Prompt Cache Optimization

- **Instructions-based cache key** — all conversations sharing the same system prompt share one cache entry, even across different conversations and agents
- **Cross-conversation cache sharing** — 10 agents with the same system prompt use 1 cache entry instead of 10
- **Async cache warming** — newly rotated accounts get a background warmup request so the first real request is already a cache hit
- **Warming dedup** — concurrent requests to the same account + cache key don't trigger duplicate warmups
- **Token threshold** — short prompts (< 1024 tokens) skip warming to avoid wasting quota
- **Cache-sticky pinning** — consecutive turns from the same conversation stay on the same account so the provider's prompt cache stays hot

### Pool Routing Strategies (20+ accounts)

For large pools with uneven quota / reset windows, two opt-in strategies improve on round-robin:

- **`cost-optimized`** — prefer accounts with the most remaining quota (lowest `CodexPrimaryUsedPercent` / highest `ExtCreditsRemaining`). Reduces mid-stream 429s.
- **`reset-aware`** — avoid accounts whose quota window resets within 30 minutes. Falls back to cost-optimized ranking among safe accounts.
- **`round-robin`** (default) — zero overhead, best for small/medium pools with even quota.

Strategies activate only when the pool has ≥ 20 unique accounts. Cache-sticky pinning always wins over strategy — a cache hit saves more quota than any strategy choice. Configure via admin panel → Usage → Pool tab, or `PATCH /admin/api/pool/strategy`.

## Note

Not all IDEs, CLI tools, and Agents are fully tested. Only Claude Code, OpenCode, and Codex are tested.

## Quick Start

### Docker Compose (Recommended)

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
mkdir -p data
docker-compose up -d
```

### Docker Run

```bash
docker run -d \
  --name omniproxy \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/mxuanvan02/omniproxy:latest
```

### Build from Source

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

### Deploy on Zeabur

The repo already includes a `Dockerfile`, so it builds and runs on Zeabur out of the box.

**Option 1: Dashboard (one-click)**

1. Fork this repo to your GitHub account.
2. In Zeabur, create a new service and choose **Deploy from GitHub**, then select your fork.
3. Zeabur auto-detects the `Dockerfile` and builds the image.
4. In the **Networking** tab, expose port `8080` and bind a domain.
5. In the **Variables** tab, set at least `ADMIN_PASSWORD` (admin panel password).
6. Mount a Volume at `/app/data` if you want accounts / config to survive redeploys.

**Option 2: CLI**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> Run the commands from the project root. The CLI writes `.zeabur/context.json` to remember the target project / service — it contains personal IDs, so don't commit it.

Once the service is up, open `https://<your-domain>/admin` to log in.

Config is auto-created at `data/config.json`. Mount `/app/data` for persistence. The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI / Chat
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI / Responses
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"claude-sonnet-4.5","input":"Hello!","max_output_tokens":1024}'
```

## Thinking Mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude-compatible requests that include a top-level `thinking` config such as `{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}` also enable thinking mode automatically. Configure output format in the admin panel under Settings - Thinking Mode.

## Outbound Proxy

For users in restricted network regions, configure an outbound proxy in the admin panel under **Settings - Outbound Proxy Settings**. Supports SOCKS5 and HTTP proxies.

The setting takes effect immediately without restarting.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |

## Project Structure

```
OmniProxy/
├── auth/           # OAuth flows for 12 auth methods
├── cli/            # CLI entry point and interactive menu
├── config/         # Config schema, persistence, KV settings
├── pool/           # Account pool, routing strategies, cache warming
├── proxy/          # HTTP handlers, translators, failover, admin API
├── web/            # Embedded admin panel (HTML/JS/CSS, i18n)
├── docs/           # Documentation
├── Dockerfile      # Multi-arch Docker build (amd64 + arm64)
├── docker-compose.yml
└── go.mod          # Module: omniproxy
```

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for release history and notable changes.

## Acknowledgements

OmniProxy is a fork of [Kiro-Go](https://github.com/Quorinex/Kiro-Go) and is developed based on it. The original project provided the foundation for Kiro account management, token refresh, and the OpenAI / Anthropic compatible API layer.

Key additions in OmniProxy beyond the upstream:

- Codex (ChatGPT subscription) account support with usage tracking
- External OpenAI-compatible provider support
- Instructions-based prompt cache key with cross-conversation sharing
- Async cache warming with dedup and token threshold
- Pool routing strategies (cost-optimized, reset-aware) for 20+ account pools
- Combo fallback chains with per-combo strategy
- Web admin panel with i18n (EN / CN / VN)
- Per-account outbound proxy (SOCKS5 / HTTP)
- Prompt filter system with custom regex rules

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE) — Copyright (c) 2026 mxuanvan02
