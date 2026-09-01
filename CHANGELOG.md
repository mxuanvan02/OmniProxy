# Changelog

All notable changes to OmniProxy are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Google Antigravity backend** (`AuthMethod: "antigravity"`) — OAuth PKCE loopback login, import of local `~/.gemini` / `~/.antigravity` credentials, per-account `cloudaicompanionProject` discovery (`loadCodeAssist` / `onboardUser`, cached 24h), streaming and non-streaming chat over `cloudcode-pa.googleapis.com`, live model catalog. A 403 without an auth phrase marks the account `BANNED` so the pool stops selecting it. **Google's Antigravity Terms of Service prohibit third-party clients and accounts have been disabled for using them** — see §7 of the README before enabling.
- **Gommo AutoAI backend** (`AuthMethod: "gommo"`) — form-urlencoded provider (also fronted by 79AI) for image, video, TTS and music generation, with async job polling, credit/balance tracking and per-image billing that keeps already-paid-for results when a later image fails. The provider cannot answer chat completions, so the `chat` capability is rejected at import.
- **Video generation endpoints** — `POST /v1/videos/generations` and `GET /v1/videos/{id}`, plus admin routes for creating and polling Gommo video jobs.
- **Native capability hook** — `tryNativeCapability` runs before the OpenAI-compatible passthrough, letting non-OpenAI-shaped providers serve `/v1/audio/speech` and `/v1/images/generations` directly.
- **Admin UI** — Antigravity (OAuth / import / local-credential detection / project refresh) and Gommo (import, capability picker, balance refresh) cards in the add-account flow, with EN / VN / ZH strings.
- **Pool routing strategies** — `cost-optimized` and `reset-aware` opt-in strategies for pools with 20+ accounts. Configurable via admin panel (Usage → Pool tab) or `PATCH /admin/api/pool/strategy`. Round-robin remains the default with zero overhead.
- **Admin API** — `GET /admin/api/pool/strategy` and `PATCH /admin/api/pool/strategy` for reading and updating the pool routing strategy at runtime.
- **Web UI** — new "Pool" tab in the Usage page with radio-card strategy selector and per-strategy descriptions.

### Changed
- `GetNextForModelExcluding` now collects filter-passing candidates and picks by strategy score when a non-default strategy is active. Round-robin keeps the original early-return path for zero overhead.
- `GetBoolSetting` / `GetStringSetting` / `SetBoolSetting` / `SetStringSetting` in `config/config.go` now nil-guard `cfg` so unit tests that bypass `config.Init` no longer panic.

### Fixed
- `accountSupportsServiceCapability` now accepts `audio-tts` and `video`. An account configured with only those capabilities previously entered no pool at all and was never selected.
- `handleImageGeneration` no longer hardcodes Codex account selection; it picks an image-capable account and dispatches through `callImageGeneration`.
- Four "non-Kiro" guards in `account_failover.go` and `handler.go` now recognise Antigravity accounts, which would otherwise be refreshed against the AWS Kiro endpoint with a Google token.

## [0.2.0] — 2026-07-18

### Added
- **Instructions-based cache key** — `payloadCacheKey` now derives the cache key from `hash(instructions)` (the system prompt) instead of `hash(conversationID)`. All conversations sharing the same system prompt share one cache entry, even across different conversations and agents.
- **Async cache warming** — newly rotated accounts receive a background warmup request so the first real request is already a cache hit. Warming runs in a goroutine and never blocks the request path.
- **Warming dedup** — a `cacheWarming` flag map prevents concurrent requests to the same account + cache key from triggering duplicate warmups.
- **Token threshold** — prompts under 1024 tokens skip warming to avoid wasting quota on short prompts that won't benefit from caching.
- **Cache-sticky pinning** — `GetNextForModelWithCacheKey` pins consecutive turns from the same conversation to the same upstream account so the provider's prompt cache stays hot. TTL: 30 minutes.
- **Codex usage tracking** — `CodexPrimaryUsedPercent`, `CodexSecondaryUsedPercent`, `CodexPrimaryResetAt`, `CodexSecondaryResetAt` fields on accounts, populated from `x-codex-*` response headers.
- **External OpenAI-compatible provider support** — `AuthMethod: "external_openai"` with credit balance tracking via `/api/me`.
- **Combo fallback chains** — per-combo strategy override (`fallback` or `round-robin`) with sticky round-robin limit.
- **Provider-aware routing** — Claude models prefer external OpenAI-compatible accounts; GPT models prefer Codex accounts.
- **Per-model cooldown** — quota/auth failures lock only the affected model, not the whole account.
- **Web admin panel** — manage accounts, settings, combos, API keys, usage, cache stats, compression, pool strategy. i18n: EN / CN / VN.
- **Per-account outbound proxy** — global or account-level SOCKS5 / HTTP proxy.
- **Prompt filter system** — custom regex rules via admin panel; built-in Claude Code CLI system prompt replacement.
- **Thinking mode** — configurable suffix trigger, output format (`reasoning_content` / `thinking` / `think`).
- **12 auth methods** — AWS Builder ID, IAM Identity Center, SSO Token, Social Login, Kiro CLI import, Kiro SSO 3-step browser login, AWS SSO Cache, Kiro Local Cache, Credentials JSON, Kiro Web Cookie, API Key (ksk_), Refresh Token.

### Changed
- **Renamed SuperKiro → OmniProxy** — module name, binary name, Docker image, all user-facing strings. The `superkiro` provider key in config files is kept for backward compatibility.
- **Go version** — bumped to 1.25+.
- **Docker build** — multi-arch (amd64 + arm64) via `docker/build-push-action`.

### Fixed
- SSE error events are now sent instead of JSON when streaming, preventing silent hangs on upstream errors.
- Auto-ban disabled; external accounts are skipped in ModelsCache; ban status is cleared on re-enable.
- Codex accounts no longer hit Kiro DNS errors on refresh.
- `temperature` / `top_p` stripped when forwarding to ChatGPT backend (it rejects these fields).

## [0.1.6] — 2026-07-01

### Fixed
- Windows only: fixed crash when users choose option 2 to run in background.

---

> Entries before 0.1.6 were tracked in the upstream [Kiro-Go](https://github.com/Quorinex/Kiro-Go) repository.
