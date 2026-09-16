# Phase 03 — Verification & deploy

## Tests to add / extend in `proxy/external_antigravity_test.go`

### Fingerprint (phase 01)
1. `TestAntigravityUserAgentCarriesCurrentIDEVersion`
   - `setAntigravityHeaders(req,"tok")` → `req.Header.Get("User-Agent")` contains `antigravity/1.23.2`.
   - Assert against `antigravityIDEVersion` (the const), not a literal, so future bumps don't rot the test.
2. Extend `TestAntigravityHeadersCarryRequiredClientMetadata`
   - Add: `Client-Metadata` header has **exactly** ideType/platform/pluginType and **no** `ideVersion` key (pins the header/body split).
3. `TestAntigravityMetadataCarriesIDEVersion`
   - `antigravityMetadata("")["ideVersion"] == antigravityIDEVersion`; `ideType=="ANTIGRAVITY"`, `pluginType=="GEMINI"`, platform non-empty.
   - `antigravityMetadata("proj")["duetProject"] == "proj"` still set.
4. `TestAntigravityChatEndpointDefaultsToProduction`
   - account with no BaseURL, no setting → `antigravityChatEndpointFor(account) == antigravityDefaultEndpoint`.
   - set `antigravityChatEndpoint` setting → returns it (use `config.SetStringSetting` in test, reset after).
   - account BaseURL set → wins over the setting.
   - Mirror the existing `TestAntigravityEndpointDefaultsToProduction` style.

### Classification (phase 02)
5. Extend `TestClassifyAntigravityFailure` cases:
   - `"capture"` 403 → `antigravityFailureOther`.
   - bare `{"status":"PERMISSION_DENIED"}` (no message) → `antigravityFailureBanned`.
   - message echoes status (`"message":"PERMISSION_DENIED"`) → `antigravityFailureBanned`.
   - explicit ban phrase → `antigravityFailureBanned` (regression guard).
   - validation body → `antigravityFailureValidation` (regression guard).
   - unparseable 403 body → `antigravityFailureBanned` (safe fallback).
6. Extend `TestAntigravityValidationDoesNotBan` with a new case:
   - `name:"unrecognized 403 message is not a ban"`, status 403,
     body `{"error":{"code":403,"message":"capture","status":"PERMISSION_DENIED"}}`,
     `wantBanned:false`, `wantVerify:""`.
   - The existing assertions (`BanStatus==""`, `Enabled` stays true) then prove no ban.

## Build & test

```sh
export GOCACHE="$PWD/.gocache"          # sandbox requires it
go build ./...
go test ./proxy/ -run 'Antigravity' -count=1
go test ./... -count=1                  # full suite; only the pre-existing
                                        # DNS-gated TestSearchAdaptersUseNativeContracts may fail
```
Note: the `proxy` package binds loopback in tests — run outside the sandbox or with
`allowLocalBinding`, per the saved memory `sandbox-loopback-blocks-httptest`.

## Deploy (must be from a NON-sandboxed shell)

```sh
./build.sh        # rebuild binary (restart.sh does NOT build)
./restart.sh      # launchctl kickstart -k — new process, clean DNS
```
Why non-sandboxed: a process started under the sandbox inherits no-DNS, which is
exactly what produced the 12:13 refresh failures. The launchd plist already carries
`ANTIGRAVITY_CLIENT_ID` / `ANTIGRAVITY_CLIENT_SECRET`; do not touch them.

## Post-deploy verification (live log: `data/omniproxy.launchd.err.log`, slash dates)

```sh
# 1. no new ban after restart
grep "disabled upstream" data/omniproxy.launchd.err.log | grep "2026/09/16 1[6-9]" | tail

# 2. token refresh succeeds (no DNS error) within one refresh cycle
grep -iE "antigravity.*refresh" data/omniproxy.launchd.err.log | grep "2026/09/16 1[6-9]" | tail

# 3. loadCodeAssist / catalog no longer 400 (MACOS already fixed; watch ideVersion)
grep -iE "loadCodeAssist|fetchAvailableModels|catalog" data/omniproxy.launchd.err.log | grep "2026/09/16 1[6-9]" | tail

# 4. chat path actually attempted (streamGenerateContent) — has been 0 historically
grep -iE "streamGenerateContent|CallExternalAntigravity" data/omniproxy.launchd.err.log | grep "2026/09/16 1[6-9]" | tail
```

Decision gate after deploy:
- If refresh + catalog succeed and a real chat request to an antigravity model
  returns 200 → fingerprint fix worked; leave `antigravityChatEndpoint` unset.
- If chat still 403s `"capture"` on `cloudcode-pa` → set
  `antigravityChatEndpoint = https://daily-cloudcode-pa.googleapis.com` (admin
  settings, no rebuild) and re-test; expect possible 503 MODEL_CAPACITY_EXHAUSTED
  (the documented daily-host tradeoff). Either way the account must **not** be
  banned (phase 02).

## Commit (conventional, no AI refs in body, shared repo — check index first)

```sh
git add proxy/external_antigravity.go proxy/external_antigravity_test.go \
        plans/20260916-1642-antigravity-login-reconfiguration
git status                      # confirm ONLY these are staged (parallel workstream!)
git diff --cached --stat        # inspect before commit
git commit -m "fix(antigravity): bump IDE fingerprint and stop banning on unknown 403"
# body: explain capture-403 was transient, ideVersion/UA aligned to current client
# trailer:
# Co-Authored-By: Claude Code <noreply@anthropic.com>
```
Pre-push hook runs `go test ./...` without GOCACHE and fails in-sandbox; push with
`--no-verify` **only after** the manual suite is green (per prior session).

## Success criteria
- All Antigravity tests pass; full suite green except the known DNS-gated test.
- Running process refreshes tokens without DNS errors.
- `"capture"` (or any unrecognized 403 message) rotates instead of banning.
- No OAuth secret in any committed file.

## Results (executed)

Two corrections to the plan above, found while verifying:

1. **The runtime err log is WARN+ only.** The success line
   `[ModelsCache] Seeded N Antigravity models` is `Info`, so it never reaches
   `data/omniproxy.launchd.err.log`. "No antigravity log line" is therefore NOT
   evidence of success — a passive tail cannot confirm this change.
2. **`loadCodeAssist` is gated by a 24h project TTL.** Both accounts had a fresh
   `antigravityProjectCheckedAt` (4–5h old), so a normal catalog tick only calls
   `fetchAvailableModels`, which carries **no** `metadata` field. The tick would
   never have exercised the new `ideVersion`.

So verification was done actively instead: `POST /admin/api/login` →
`POST /admin/api/accounts/{id}/antigravity-project`, which sets
`AntigravityProjectCheckedAt = 0` and calls `ensureAntigravityProject` →
a real `loadCodeAssist` to Google on the path that now carries `ideVersion`.

- `neihhh04@gmail.com` → HTTP 200 `{"projectId":"aicode-consumers","success":true,"tier":"standard-tier"}`
- `mxuanhien2@gmail.com` → HTTP 200, same shape

That proves in one shot: the new process resolves DNS and reaches Google; token
refresh works; and Google **accepts** `metadata.ideVersion = "1.23.2"` (no 400).
Post-check: zero `disabled upstream` / `no such host` / 400 lines since the 17:09
restart, both accounts `banStatus=ACTIVE`, `enabled=true`.

Chat-path fingerprint (`"capture"` 403) is covered by tests, not yet by live
traffic — no antigravity chat request has been routed since the restart. Phase 02
guarantees that if `"capture"` returns, the account rotates instead of being
banned. The `antigravityChatEndpoint` setting stays unset (production host); flip
it only if live chat keeps 403-ing.
