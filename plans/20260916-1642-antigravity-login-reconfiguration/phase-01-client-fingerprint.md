# Phase 01 — Client fingerprint (IDE version + chat host)

## Context

- Reference: 9router v0.5.45 `app/src/mitm/server.js`
  - `ANTIGRAVITY_IDE_VERSION = "1.23.2"` (`mr`)
  - `rewriteAntigravityUserAgent(ua, ver)`: `ua.replace(/antigravity\/[^\s]+/, "antigravity/1.23.2")`
  - `applyAntigravityIdeVersionOverride(body, headers)`: sets `body.metadata.ideVersion = "1.23.2"`
    when the metadata is antigravity-shaped (`ideType == "ANTIGRAVITY"` qualifies).
- OmniProxy: `proxy/external_antigravity.go`
  - `antigravityIDEVersion = "1.18.3"` (:51), used only in `setAntigravityHeaders` UA (:107).
  - `antigravityMetadata(projectID)` (:375) → `{ideType,platform,pluginType[,duetProject]}` — no `ideVersion`.
  - `antigravityEndpoint(account)` (:66) → per-account `BaseURL` override, else `cloudcode-pa`. Used by BOTH chat (:1154) and control-plane (:556).
  - Chat body (`kiroPayloadToAntigravityRequest`, return at :868) = `{project, model, request, userAgent, requestId}` — **no `metadata`**.

## Why this is safe / honest

The IDE version string is a real published Antigravity version, kept fixed. Bumping
1.18.3 → 1.23.2 is "report the current real client version", not rotating
fingerprint or synthetic telemetry — it stays inside the existing code posture
(`antigravity_oauth.go` header comment). No new headers, no per-request jitter.

## Changes

### 1. Bump the IDE version constant
`proxy/external_antigravity.go:51`
```go
const antigravityIDEVersion = "1.23.2"
```
This flows to the UA on every request via `setAntigravityHeaders` (already wired).

### 2. Add `ideVersion` to control-plane metadata
`proxy/external_antigravity.go:375` `antigravityMetadata`:
```go
metadata := map[string]string{
	"ideType":    "ANTIGRAVITY",
	"platform":   antigravityPlatform(),
	"pluginType": "GEMINI",
	"ideVersion": antigravityIDEVersion,
}
```
Only loadCodeAssist (:495) and onboardUser (:515) use this; both already accept a
`metadata` object. Chat does NOT use it (no metadata key) — leave chat body alone.
The `Client-Metadata` **header** (:109) stays `{ideType,platform,pluginType}` to
match 9router's `loadCodeAssistClientMetadata` exactly — do not add ideVersion there.

### 3. Chat host split (opt-in, default unchanged)
9router sends chat to `daily-cloudcode-pa` and control-plane to `cloudcode-pa`.
OmniProxy's `cloudcode-pa` comment (:43) warns the daily host is capacity-starved
(503 MODEL_CAPACITY_EXHAUSTED). So: **do not** flip the default. Add an opt-in so
the daily host can be tried per-deployment without a code edit.

Add near the endpoint const:
```go
// antigravityChatEndpoint resolves the base URL for the streaming chat action.
// It defaults to the same production host as the control plane; set
// antigravityChatEndpointSetting to route chat elsewhere (9router splits chat to
// daily-cloudcode-pa). A per-account BaseURL still wins for operators who pin it.
const antigravityChatEndpointSetting = "antigravityChatEndpoint"

func antigravityChatEndpointFor(account *config.Account) string {
	if account != nil {
		if base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/"); base != "" {
			return base
		}
	}
	if v := strings.TrimRight(strings.TrimSpace(config.GetStringSetting(antigravityChatEndpointSetting, "")), "/"); v != "" {
		return v
	}
	return antigravityDefaultEndpoint
}
```
Change the chat builder (:1154) only:
```go
endpoint := antigravityChatEndpointFor(account) + antigravityStreamAction
```
Control-plane `antigravityPostJSON` (:556) keeps `antigravityEndpoint(account)` — unchanged.

## Tests (see phase 03 for full list)
- `setAntigravityHeaders` UA contains `antigravity/1.23.2`.
- `antigravityMetadata("")["ideVersion"] == "1.23.2"`.
- `Client-Metadata` header still has exactly ideType/platform/pluginType (no ideVersion).
- chat endpoint default == production; setting override changes chat only; account BaseURL still wins.

## Success criteria
- Compiles; existing `TestAntigravityHeadersCarryRequiredClientMetadata` and
  platform tests still pass (update the version assertion if it pins 1.18.3).
- No behavior change unless `antigravityChatEndpoint` setting is set.

## Risks
- A test may hardcode `1.18.3` — update it to the new constant, not a literal.
- `ideVersion` in metadata: if Google ever rejects an unknown field on
  loadCodeAssist, it 400s. Mitigation: 9router injects exactly this field on the
  same endpoint, so it is a known-accepted key.
