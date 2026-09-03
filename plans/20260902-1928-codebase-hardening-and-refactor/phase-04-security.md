# Phase 04 — Security hardening

**Context:** [plan.md](plan.md) · source: security review (C1–C3, H1–H4, M1–M4)
**Priority:** P0 · **Status:** ⬜ pending · **Group:** B — runs after phase 01 (both touch `proxy/handler.go`)

## Overview

A first-run install is LAN-exposed with a known password and client auth off. Admin password is compared with `!=`, accepted via URL query, and the API sets wildcard CORS globally — so a page the operator visits can read every stored upstream OAuth token cross-origin. Credential files are written 0644.

## Key insights

- **C2 is the root cause; C1 and C3 are the amplifiers.** Fix the defaults first — that alone downgrades the rest from "remote takeover" to "needs a guessed secret".
- `?pwd=` **cannot simply be deleted**: `EventSource` cannot set headers, and two front-end call sites depend on it (`web/logs.js:168`, `web/usage.js:215`). Replace with a short-lived random session token that is *not* the password, still passed as a query param for the SSE endpoints only.
- `crypto/subtle` appears nowhere in the repo. Admin password (`handler.go:5278`), client API key (`config/apikeys.go:141`), legacy key (`proxy/auth.go:91`) all use `==`.
- Codebase already knows the right file mode: `config/config.go:805` and `proxy/responses_store.go:74` use 0600. The ~12 CLI-tool credential writers in `handler.go:5751-6093` use 0644/0755. Inconsistency, not ignorance.
- `Allow-Credentials` is **not** set, so the cookie path is not directly cross-origin exploitable — the `?pwd=` query path is what makes C3 live.
- Empty password is currently a *valid* password: `cfg.Password == ""` + a request with no credential at all → `"" != ""` is false → authorized (`handler.go:5271-5278`).
- `jina-reader` (`proxy/search.go:236-243`) takes a client-supplied URL and validates only non-empty scheme/host — the one SSRF path reachable without admin access.

## Requirements

Functional
- Fresh config binds loopback; `0.0.0.0` requires explicit opt-in.
- Startup refuses `changeme` and empty password; generates a random one on first run and prints it once.
- Admin API accepts a session token (header, or query for SSE only); password itself is accepted only at login.
- Dashboard keeps working: login, logs stream, usage stream.
- Credential files 0600, dirs 0700.

Non-functional
- No new dependency. `crypto/rand` + `crypto/subtle` are stdlib.
- Existing deployments must not be locked out silently — an already-set non-default password keeps working.

## Architecture

```
POST /admin/api/login  {password}
      │ subtle.ConstantTimeCompare
      ▼
  session token (32B crypto/rand, TTL, in-memory map)
      │
      ├─ XHR/fetch  → X-Admin-Token header
      └─ EventSource → ?token=…   (SSE endpoints only)
                            │
                     /admin/api/*  ── ConstantTimeCompare ──▶ handler
```

CORS: wildcard stays on `/v1/*` (clients need it); `/admin/api/*` echoes only same-origin / loopback.

## Related code files

Modify
- `config/config.go` — default `Host` → `127.0.0.1`; reject empty/default password; generate first-run secret
- `main.go` — startup refusal + one-time password print; `ADMIN_PASSWORD` floor
- `proxy/handler.go` — `handleAdminAPI` auth rewrite (`:5265-5282`), CORS scoping (`:1405-1408`), `serveStaticFile` (`:11196-11200`), file modes (`:5751-6093`, `:630`)
- `proxy/auth.go` — constant-time client key compare (`:91`)
- `config/apikeys.go` — constant-time compare (`:141`)
- `proxy/search.go` — SSRF allowlist, starting with jina-reader (`:236-243`)
- `proxy/usage_tracker.go` — usage file 0600 (`:304`, `:307`)
- `web/app.js`, `web/logs.js`, `web/usage.js` — token flow

Create — none (session store lives in the `Handler` struct).

## Implementation steps

1. **C2 defaults.** `config/config.go:600-603`: `Host: "127.0.0.1"`. Keep `GetHost()`'s empty-string fallback. Document the `0.0.0.0` opt-in for Docker in the compose comments (phase 02 owns that file — coordinate, do not edit it here).
2. **M3 + C2 password floor.** On load: if `Password` is `""` or `"changeme"`, generate 24 random bytes → base64, persist, and print once to stdout with a "save this" notice. Reject an explicitly configured empty password at `UpdateSettings`/`UpdateSettingsPatch` (`config.go:1519-1546`) rather than silently skipping. Same floor for `ADMIN_PASSWORD` (`main.go:62-64`).
3. **C1 session tokens.** Add `/admin/api/login` that constant-time-compares the password and returns a token; store tokens in a mutex-guarded map on `Handler` with a TTL and a cap. Rewrite `handleAdminAPI` to accept `X-Admin-Token` (header) or `?token=` (only for the two SSE routes), both via `subtle.ConstantTimeCompare`. Drop `?pwd=` and the `admin_password` cookie entirely.
4. **C3 CORS.** Move the wildcard so it applies to `/v1/*` and public routes only; for `/admin/api/*` echo the request `Origin` only when it is same-origin or loopback, else omit the header. Never set `Allow-Credentials: true`.
5. **H2 constant-time.** `subtle.ConstantTimeCompare` at `proxy/auth.go:91` and `config/apikeys.go:141`. Compare fixed-length digests if the raw lengths differ, so length itself does not leak.
6. **H4 file modes.** Every credential write in `handler.go:5751-6093` (VS Code secrets, `~/.codex/auth.json`, Kilo, DeepSeek/JCode TOML, 9router `.env`, Hermes, Droid, OpenClaw) → 0600, parent dirs 0700; config backup at `:630` → 0600; usage file (`usage_tracker.go:304`,`:307`) → 0600. Coordinate with phase 01 step 7, which rewrites that same flush — apply the mode there if 01 already landed.
7. **H1 static handler.** Replace `serveStaticFile`'s `TrimPrefix`+`Join` with `http.FileServer(http.Dir(h.webDir))`, or `filepath.Clean` + re-verify the prefix. Not currently exploitable (`http.ServeFile` rejects `..` in `r.URL.Path`) — this removes the footgun.
8. **H3 SSRF.** Add a shared validator: scheme must be http/https; reject loopback, link-local (169.254/16), and RFC1918 after DNS resolution. Apply to `proxy/search.go:236-243` first (raw client input), then `:225`, `:239`, `:366`, `proxy/capability_endpoints.go:282`, `proxy/images.go:243`, `:484`, `:514`. Admin-configured base URLs are lower priority than the client-supplied path.
9. **Frontend.** `web/app.js`: login posts the password once, stores the returned token (sessionStorage; keep the existing remember-me path but store the token, never the password), sends `X-Admin-Token`. `web/logs.js:168` / `web/usage.js:215`: `?token=` instead of `?pwd=`. Handle 401 → clear token → re-prompt.
10. **M1 audit.** Re-enumerate unauthenticated routes after the rewrite and record the intended list in the phase notes: `/health`, `/`, `/admin` static are deliberate; decide whether `/v1/models` (leaks the account/model inventory) and `/v1/stats` (leaks credit balances) should require a client key even when `RequireApiKey=false`.
11. Build / vet / `-race` after each of steps 3, 4, 6, 8. Then click through every dashboard tab, including logs + usage streams.

## Todo

- [x] Default `Host` loopback
- [x] `changeme`/empty rejected; random first-run password printed once
- [x] `/admin/api/login` + session tokens; `?pwd=` and cookie removed
- [x] CORS scoped off `/admin/api/*`
- [x] `subtle.ConstantTimeCompare` for password, client key, legacy key
- [x] All credential writes 0600 / dirs 0700
- [x] `serveStaticFile` → `http.FileServer`
- [x] SSRF validator on jina-reader — remaining builders deliberately skipped (see Decisions)
- [x] Frontend token flow (app.js, logs.js, usage.js, settings.js)
- [x] Unauthenticated-route list decided + recorded (see Decisions)
- [x] CSP + `nosniff` / `no-referrer` / `DENY` on both admin static handlers
- [x] build / vet / `-race` green — **dashboard click-through unverified** (no browser in env)

## Decisions

**Unauthenticated routes, intentional:** `/health`, `/`, `/admin` + `/admin/*` static, `/api/event_logging/batch` (telemetry sink, returns a constant), `/v1/models`, `/v1/models/{id}`. Everything under `/v1/*` that dispatches upstream requires a client key when `RequireApiKey=true`; `/v1/stats` requires one unconditionally. `/v1/models` stays open because `web/combos.js:285-286` fetches it with no admin token — gating it would break the combos tab. It leaks the model inventory, not credentials or balances. Filed as follow-up, not fixed here.

**SSRF scope:** only `proxy/search.go:239-243` (jina-reader) takes a client-supplied URL, and it is now validated. `search.go:229`/`:369`, `capability_endpoints.go:284`, `images.go:211`/`:514` all build from `account.BaseURL`, which only an authenticated admin sets — blocking private IPs there would break a self-hosted gateway on a LAN address, so they are left alone. `allowPrivateOutbound` exists for the same reason.

**CSP keeps `'unsafe-inline'`** for `script-src`/`style-src`: the templates carry ~90 inline `onclick` handlers and 2 inline `<script>` blocks. External origins still cannot supply script, and `connect-src` limits exfiltration to `'self'` + `raw.githubusercontent.com` (the update check). Nonces need the handlers removed first — phase 07 territory.

**`ADMIN_PASSWORD` / generated password are never logged**: `main.go` prints to stderr, `cli/cli.go` writes to the daemon log file directly. `logger.*` would put the secret in the ring buffer that `/admin/api/logs` replays to any authenticated caller.

## Success criteria

- Fresh `data/` → binds 127.0.0.1, password is random, printed once.
- `curl 'http://127.0.0.1:8080/admin/api/accounts?pwd=<pw>'` → 401.
- No `Access-Control-Allow-Origin: *` on any `/admin/api/*` response.
- `grep -rn 'crypto/subtle'` shows all three comparison sites.
- Every file written under `handler.go:5751-6093` is `-rw-------`.
- Dashboard login, logs stream, usage stream all work.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| Operator locked out of a live deployment by the password floor | Only `""`/`changeme` trigger regeneration; any real password is untouched. Print the new one prominently |
| Loopback default breaks an existing Docker deployment | Existing configs keep their stored `Host`; only *new* configs default to loopback. Compose ships the opt-in |
| SSE streams break (EventSource cannot send headers) | Token-in-query retained for exactly those two routes; tested in step 11 |
| Token map grows unbounded | TTL + size cap; evict on expiry |
| Overlap with phase 01 in `handler.go` / `usage_tracker.go` | 04 runs after 01, not concurrently; re-read those files before editing |
| SSRF allowlist breaks a legitimate self-hosted gateway on a private IP | Make the private-IP block configurable, default on; log the rejection with the resolved address |

## Security considerations

This is the security phase; the whole content is the consideration. Two things to keep in mind while implementing:
- Do not log the generated password anywhere except the one-time stdout notice, and never log tokens. `/admin/api/logs` replays the whole ring buffer to any authenticated caller, so any interpolated secret becomes remotely readable — add a redaction helper in `logger/` while here (M2).
- Do not read or echo `data/config.json` contents into responses beyond what the dashboard already needs; `handler.go:10883-10884` currently returns `accessToken`/`refreshToken`/`clientSecret` in cleartext. Masking them is out of scope for this phase but should be filed as a follow-up.

## Next steps

Phases 05 + 06 run concurrently after this. Follow-up to file: mask token fields in the accounts API response.
