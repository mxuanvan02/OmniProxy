# Changelog

All notable changes to OmniProxy are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security
- **Admin auth is now a session token.** `POST /admin/api/login` exchanges the password for a 12-hour token compared with `crypto/subtle.ConstantTimeCompare`; every other admin route requires it in `X-Admin-Token`. The `?pwd=` query parameter and the `admin_password` cookie are gone — a password could previously leak through referrers, proxy access logs and browser history. The two SSE routes (`/admin/api/logs/stream`, `/admin/api/usage/stream`) accept `?token=` because `EventSource` cannot set headers. The dashboard no longer stores the password anywhere; legacy `admin_password` / `kiro_remembered_pwd` storage keys are purged on load.
- **CORS no longer wildcards the admin API.** `/admin/api/*` echoes `Access-Control-Allow-Origin` only for same-origin or loopback requests; `/v1/*` keeps `*` for API clients. The admin API returns every stored upstream OAuth token, so any page the operator visited could previously read them cross-origin.
- **Default bind is `127.0.0.1`.** A fresh install is no longer reachable from the LAN. Existing configs keep their stored `host`; containers opt back in via `docker-compose.yml`.
- **`changeme` and blank admin passwords are refused.** A 24-byte random password is generated on first run and printed once to stderr (never through `logger`, whose ring buffer is replayed by `/admin/api/logs`). `ADMIN_PASSWORD` is held to the same floor, and the check trims first, so `" "` is rejected as blank rather than stored as a one-space secret.
- **Changing the admin password revokes every existing session.** An operator rotating a password they believe compromised previously left the attacker's token valid for its full 12-hour life; only the browser that made the change re-authenticated.
- Client API keys and the legacy key are compared with `crypto/subtle` over SHA-256 digests, so neither the value nor its length leaks through timing.
- Credential files written by the CLI-tool integrations (VS Code secrets, `~/.codex/auth.json`, Kilo, DeepSeek/JCode, 9router, Hermes, Droid, OpenClaw) and config backups are `0600`, their directories `0700` — previously `0644`/`0755`.
- SSRF guard on the one client-supplied URL (`jina-reader`): scheme must be http/https, and loopback / RFC1918 / CGNAT / link-local targets are rejected after DNS resolution, so cloud metadata endpoints are unreachable.
- `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY` on the admin pages; static assets are served by `http.FileServer`, which cannot be walked out of its root.
- Request bodies on the inference paths are capped at 64 MiB and answered with 413 instead of being buffered unbounded.

### Fixed
- **SOTA aliases no longer inherit fabricated Claude 5 metadata.** `model-S`/`model-T`/`model-O`/`model-A` were treated as 1M/128K Claude 5 models, and Claude Code picker rows were given `behavesAs` profiles they never advertised. Catalog metadata from the gateway now wins; missing metadata stays unset.
- **Concurrent map read/write crash.** `SetBoolSetting` / `SetStringSetting` released `cfgLock` before calling `Save()`, so a concurrent setting write could race the marshal and kill the process with an unrecoverable runtime throw.
- **Account pool handed out pointers into its own slice.** Every selector now returns a copy; callers writing `AccessToken` / `ExpiresAt` through the returned account could previously corrupt a token mid-refresh or lose the update entirely when `Reload()` replaced the slice.
- **A panic in any spawned goroutine took down the whole proxy**, dropping every in-flight stream. `net/http` only recovers its own handler goroutines; all 14 self-spawned goroutines now run under a `safeGo` recover guard that logs the panic and its stack.
- Compression settings are read through an immutable snapshot taken under the lock, so a live settings change can no longer tear a string field mid-request; `InitCompressionConfig` and the admin read path hold the right mutex.
- Capability routing no longer holds the pool write lock across a config read — it took `Lock` for a read-only selection and then acquired `cfgLock`, serialising all capability routing against chat routing.
- The bulk account refresh patches only token and ban fields, instead of replacing the whole record and clobbering usage counters a live request had just bumped.
- Usage flush no longer discards write errors or clears its dirty flag on failure (a full disk silently dropped records permanently), and writes temp+rename so a crash mid-write cannot truncate `usage_daily.json`.
- **Client disconnects now cancel the upstream request.** Every chat path derives from `r.Context()`, so an abandoned stream releases its account immediately instead of holding it — and billing tokens — until the 15-minute idle timeout. The failover loop recognises a cancellation as the client leaving rather than an account fault, so one disconnect no longer walks the pool recording a failure against every account.
- Changing the admin password revokes every outstanding session token; previously a rotation left existing tokens valid for their full 12 hours.
- The usage flush writes to a uniquely-named temp file, so a shutdown flush racing the 30-second ticker can no longer splice two writes into one corrupt file that is then silently discarded on load.
- A compression settings change persists after releasing the settings lock, instead of holding it across six fsynced config writes while every in-flight request waits to read those settings.
- The Codex branch of the bulk refresh patches only the ban fields, matching the Kiro branch; it previously rewrote the whole record and reset usage counters that live requests had bumped during the pass.
- **Usage charts read UTC**, matching the UTC storage keys. In a non-UTC zone the chart's "today" column and `?period=today` disagreed nightly. Separately, the hourly "today" window was filled from *yesterday*: it snapped to midnight and then subtracted 24 hours.
- Clean shutdown stops the background loops and flushes usage, stats and any coalesced config save; the final 30-second window was previously lost.
- sqlite auto-import applies `busy_timeout` via `_pragma` (the `_busy_timeout` DSN form is silently ignored by `modernc.org/sqlite`), limits itself to one connection, and reports a partial table listing as an error instead of "key absent".

### Added
- GitHub Actions CI runs `go build`, `go vet` and `go test -race` on every push to `main` and every PR — previously nothing verified Go code.
- Tests: `auth` 15.0% → 50.2%, `pool` 59.9% → 88.2% coverage, covering OAuth refresh/exchange/decode for every provider, the model-aware pool selector, error recording and cooldowns, and the CLI-config writers.

### Changed
- Docker: pinned `alpine:3.21` runtime, non-root `65532`, healthcheck via the bundled busybox `wget`; `docker-compose.yml` drops the obsolete `version:` key.
- `web/escape.js` holds the single `escapeHtml` / `escapeAttr` implementation, replacing two divergent copies; the unreferenced 2 MB `web/icon.svg` is deleted.

## [0.4.0] — 2026-09-02

### Added
- **Google Antigravity backend** (`AuthMethod: "antigravity"`) — OAuth PKCE loopback login, import of local `~/.gemini` / `~/.antigravity` credentials, per-account `cloudaicompanionProject` discovery (`loadCodeAssist` / `onboardUser`, cached 24h), streaming and non-streaming chat over `cloudcode-pa.googleapis.com`, live model catalog. A 403 without an auth phrase marks the account `BANNED` so the pool stops selecting it. The desktop OAuth client is not shipped: the login reads `ANTIGRAVITY_CLIENT_ID` / `ANTIGRAVITY_CLIENT_SECRET` (or the matching settings) and fails with a clear message when neither is set. **Google's Antigravity Terms of Service prohibit third-party clients and accounts have been disabled for using them** — see §7 of the README before enabling.
- **Gommo AutoAI backend** (`AuthMethod: "gommo"`) — form-urlencoded provider (also fronted by 79AI) for image, video, TTS and music generation, with async job polling, credit/balance tracking and per-image billing that keeps already-paid-for results when a later image fails. The provider cannot answer chat completions, so the `chat` capability is rejected at import.
- **Video generation endpoints** — `POST /v1/videos/generations` and `GET /v1/videos/{id}`, plus admin routes for creating and polling Gommo video jobs.
- **Music generation endpoints** — `POST /v1/music/generations` and `GET /v1/music/{id}` behind the `audio-music` capability, with the song-name and styles floors upstream enforces checked locally so a rejected request costs no round-trip.
- **Gommo media playground** — an admin modal that runs image, video, speech and music generation, and both job lookups, against one named account instead of the pool, so a credential can be verified the moment it is added.
- **Editable Gommo account settings** — capabilities, default speech model and default voice ID can be changed after import. Capabilities partition the pool, so an account whose upstream gained music support previously stayed `503` on `/v1/music/generations` for its whole life.
- **Native capability hook** — `tryNativeCapability` runs before the OpenAI-compatible passthrough, letting non-OpenAI-shaped providers serve `/v1/audio/speech` and `/v1/images/generations` directly.
- **Admin UI** — Antigravity (OAuth / import / local-credential detection / project refresh) and Gommo (import, capability picker, balance refresh) cards in the add-account flow, with EN / VN / ZH strings.
- **Pool routing strategies** — `cost-optimized` and `reset-aware` opt-in strategies for pools with 20+ accounts. Configurable via admin panel (Usage → Pool tab) or `PATCH /admin/api/pool/strategy`. Round-robin remains the default with zero overhead.
- **Admin API** — `GET /admin/api/pool/strategy` and `PATCH /admin/api/pool/strategy` for reading and updating the pool routing strategy at runtime.
- **Web UI** — new "Pool" tab in the Usage page with radio-card strategy selector and per-strategy descriptions.

### Changed
- `GetNextForModelExcluding` now collects filter-passing candidates and picks by strategy score when a non-default strategy is active. Round-robin keeps the original early-return path for zero overhead.
- `GetBoolSetting` / `GetStringSetting` / `SetBoolSetting` / `SetStringSetting` in `config/config.go` now nil-guard `cfg` so unit tests that bypass `config.Init` no longer panic.
- Capability probing separates *advertised* (listed in a provider catalog) from *verified* (an endpoint answered), and keeps "skipped" distinct from "failed" — a probe that never left the process says nothing about the endpoint.
- The admin account list groups by configured capability rather than discovered, with collapsible provider groups and external accounts subgrouped by base URL.
- The Details tab streams over SSE deltas and polled endpoints revalidate with ETags, cutting render churn on large pools.

### Fixed
- `accountSupportsServiceCapability` now accepts `audio-tts` and `video`. An account configured with only those capabilities previously entered no pool at all and was never selected.
- Gommo job status replies are read from their nested envelope (`videoInfo` / `musicInfo`), tolerate the `null` and empty-array shapes returned for an unknown id, and answer `404` instead of reporting a finished render with a blank URL.
- The Gommo playground no longer shows duplicate job-lookup tabs, empty or unusable model dropdowns, or untranslated i18n keys, and now sends the song name and lyrics the handler already parsed.
- Prompt-cache configuration is aligned with provider docs, unprimed conversations are keyed separately, and sticky pinning survives past the cache window.
- Streaming: metadata-only SSE is bounded and HTTP/2 stream resets are retried instead of surfacing as a hang.
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
