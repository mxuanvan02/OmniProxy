# Antigravity login reconfiguration (9router reference)

## Goal

Re-audit the Antigravity (Cloud Code Assist) auth path against 9router v0.5.45
and fix what is actually broken. The login/OAuth wiring is already correct — the
real blocker is on the **request fingerprint** and **403 classification**, which
the user approved fixing ("Cả hai: fingerprint + phân loại", "bump UA + ideVersion").

## What the audit found (evidence)

| Item | OmniProxy today | 9router v0.5.45 | Verdict |
|---|---|---|---|
| OAuth client ID | set in launchd env (`ANTIGRAVITY_CLIENT_ID`) | same desktop client | **match** |
| authorize/token URL | `accounts.google.com/o/oauth2/auth`, `oauth2.googleapis.com/token` | same | **match** |
| scopes | cloud-platform, userinfo.email/profile, cclog, experimentsandconfigs | same 5 | **match** |
| PKCE | S256, `access_type=offline`, `prompt=consent` | same | **match** |
| Client-Metadata | `{ideType,platform,pluginType}` | `loadCodeAssistClientMetadata` same | **match** |
| platform enum | `DARWIN_ARM64` (arch-qualified) | numeric `2` (same enum) | **match** |
| `"MACOS"` 400 | fixed in `502062e` | n/a | **history, resolved** |
| IDE version | `1.18.3` | `ANTIGRAVITY_IDE_VERSION = 1.23.2` | **delta** |
| body `metadata.ideVersion` | absent on control-plane bodies | injected `1.23.2` | **delta** |
| chat host | `cloudcode-pa` (all actions) | `daily-cloudcode-pa` (chat) / `cloudcode-pa` (control) | **delta (split)** |
| `403 {"message":"capture"}` | → `antigravityFailureBanned` (terminal) | n/a (MITM avoids it) | **over-punishes** |

Key facts:
- Chat body = `{project, model, request, userAgent, requestId}` — **no `metadata`**,
  so 9router's body `ideVersion` injection only applies to control-plane calls.
- 9router marks its direct-API `antigravity` provider `deprecated:true /
  RISK_NOTICE`; its robust path is MITM (DNS-hijack + root CA + TLS intercept).
  OmniProxy has no MITM (no new Go deps + no telemetry-impersonation posture), so
  we close the gap as far as a direct client honestly can — not by becoming MITM.
- The 12:13 DNS refresh failures were a **previous** sandboxed process; the live
  process (20-min uptime) has 0 network errors. Fix is `./restart.sh` from a
  non-sandboxed shell, not code.

## Phases

- [x] **01 — client fingerprint** → `phase-01-client-fingerprint.md`
      Bump `antigravityIDEVersion` 1.18.3 → 1.23.2 (UA on every request); add
      `ideVersion` to control-plane `antigravityMetadata()`; make the **chat host**
      opt-in via `antigravityChatEndpoint` (default unchanged: production), so the
      daily host can be tested without a code change and without adopting a host
      the codebase already documents as capacity-starved.
- [x] **02 — 403 classification** → `phase-02-403-classification.md`
      Stop treating an *unrecognized, non-empty* 403 message (e.g. `"capture"`) as
      a terminal ban. Explicit ban phrases and a *bare* PERMISSION_DENIED stay
      terminal; unknown phrases become recoverable (`antigravityFailureOther`) so
      the account is not switched off on a string we cannot verify.
- [x] **03 — verification & deploy** → `phase-03-verification-deploy.md`
      Tests added and green (full suite passes except the pre-existing DNS-gated
      `TestSearchAdaptersUseNativeContracts`); binary rebuilt + restarted from a
      non-sandboxed shell (PID 50001, health 200, 0 DNS errors). Confirmed live:
      forcing `loadCodeAssist` for both accounts (admin refresh-project, bypasses
      the 24h project TTL) returned HTTP 200 `{success:true, projectId:
      aicode-consumers}` — Google accepts the new `ideVersion:1.23.2` body
      metadata, no 400, no ban, both accounts ACTIVE.

## Files

- modify `proxy/external_antigravity.go` (version const, metadata, classify, chat endpoint)
- modify `proxy/external_antigravity_test.go` (classification + fingerprint + endpoint tests)
- **no change** to `auth/antigravity_oauth.go` — login audited and already correct
- new `plans/20260916-1642-antigravity-login-reconfiguration/*`

## Explicitly out of scope

- MITM (DNS hijack, root CA, TLS intercept) — excluded by no-new-deps + posture.
- Synthetic/rotating telemetry beyond the real IDE version string.
- Refactoring `external_antigravity.go` (1433 lines, pre-existing) — smallest change only.
- Committing OAuth secrets — they stay in launchd env / settings, never in git.
