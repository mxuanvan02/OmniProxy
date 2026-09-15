# Providers view — per-vendor management page for the OmniProxy admin UI

**Date:** 2026-09-15
**Status:** Design approved, implementation plan pending
**Target:** `web-next` (React 19 admin client served at `/admin-next/`) + two focused Go changes

---

## 1. Problem

The admin UI can show every account, but not every *vendor*. When a provider goes
bad — a gateway that starts returning `403 Authentication failed` on one model,
a host whose credentials are dead, an endpoint whose wallet ran empty — the
operator sees a flat list of 70+ accounts and has to infer which upstream is at
fault.

The request: a dedicated page per provider showing the API link, how many calls
it served, and what its errors are.

"Provider" here means **vendor**, i.e. one upstream `baseUrl`
(`api.apiforcode.com`, `AgentRouter`, `kiro.pix4k.com`, …). Accounts that are not
external OpenAI-compatible providers (Kiro, Codex, Antigravity, Gommo) have no
`baseUrl` (config/config.go:130-134); they fall back to their
`provider`/`providerKind` and appear as vendor rows on the same page.

## 2. Current state

Two admin clients exist side by side:

| Client | Stack | Served at | State |
|---|---|---|---|
| `web/` | vanilla JS, 6 tabs | `/admin/` | in production, 19k lines, no types, no tests |
| `web-next/` | React 19 + TS + Tailwind 4 + Vite | `/admin-next/` | full parity, vitest tests, embedded in the Go binary via `webnext/dist` |

`web-next` is the target. `/admin/` is not touched — this change introduces no
regression risk there.

Verified live during design: `/admin-next/` returns 200 and all three built
assets resolve.

## 3. Data source map

`A` = `GET /admin/api/accounts` (ETag-wrapped, proxy/handler.go:6162).
`U` = `GET /admin/api/usage/stats?period=P` (ETag-wrapped, ~205 KiB,
proxy/handler.go:6418).
`H` = the new `GET /admin/api/pool/health` (§4B).

| Page element | Source today | Status |
|---|---|---|
| Vendor identity (group key) | No vendor key exists. `A[].baseUrl` → fallback `provider`, `providerKind`, `authMethod`, `sourceId` | Derived client-side |
| API link | `A[].baseUrl` | Available |
| Account roster + health | `A[]` filtered by group key; `enabled`, `banStatus`, `banReason`, `catalogState`, `catalogError`, `extStatus` | Available |
| Calls — lifetime | `A[].requestCount` + `serviceRequestCount` | Available |
| Calls — windowed (period P) | `U.byAccount[accountId].requests`, joined to vendor via `A`, then summed | Available (one client join) |
| Tokens / cost — windowed | `U.byAccount[id].promptTokens` / `.completionTokens` / `.realCost` | Available |
| Models served | `A[].modelCount` + `catalogState`/`catalogSource`/`catalogError`/`catalogCheckedAt` | Available |
| Recent errors | `U.recentRequests[]` filtered `status === 'error'` | Available, ring-capped at 500 |
| Errors — lifetime | `A[].errorCount` + `serviceErrorCount` + `serviceQuotaErrorCount` | Available, cumulative |
| **Errors — windowed** | — | **§4A (new)** |
| **Cooldown / locked model / "vendor đang ốm"** | — | **§4B (new)** |

Three traps found during recon, each of which would silently produce a wrong page:

1. **`recentRequests[].provider` is not the vendor.** It is the coarse family
   label from `resolveAccountMeta` (proxy/handler.go:4778-4801), which collapses
   every external vendor into `"External OpenAI"` or `"AgentRouter"`. Attribution
   must join on `accountId` against `A`, never read `.provider`.
2. **`QuotaOverview.providers` is not per-vendor either.** It buckets into four
   hardcoded auth families (proxy/admin_quota_cache.go:104-109) and sends every
   account with a non-empty BaseURL into a single `"external"` bucket. Usable as a
   secondary card; never label it per-vendor.
3. **`errorCount` (config, cumulative) ≠ `errorCounts` (pool, consecutive).**
   The UI must never label the cumulative figure "lỗi liên tiếp".

## 4. Backend changes

### 4A. Windowed error counts — `proxy/usage_tracker.go`

`PeriodSummary` (:81-107) carries no error dimension: failed requests are written
to the 500-record ring with `Status: statusError` and then dropped by the daily
rollup. Without this change the only honest error figure is the cumulative one.

Three edits:

1. Add `Errors int \`json:"errors,omitempty"\`` to `PeriodSummary`.
2. In `addToSummaryMap` (:730), increment when the record failed.
3. In `mergeSummaryMapInto` (:762), add `d.Errors += s.Errors`.

**Edit 3 is not optional and is the reason this is called out.** There are two
merge functions. `addToSummaryMap` writes into the per-day buckets (:472-478);
`mergeSummaryMapInto` folds those day buckets into the requested period
(:828-831). Patching only the first makes 24h correct and 7d/30d silently report
zero errors — the failure looks like "no errors", which is the worst possible
wrong answer for this feature.

Because `PeriodSummary` is the element type of all four breakdown maps
(`ByModel`, `ByAccount`, `ByAPIKey`, `ByEndpoint`), per-model and per-API-key
error counts follow for free. No new route: the existing `?period=P` request now
answers both.

Accepted period values (proxy/usage_tracker.go:928-946, default `24h`): `all`,
`today`, `24h`, `7d`, `30d`, `60d`.

**Known limitation to document in the UI:** daily buckets persisted before this
field exists carry no error counts, exactly as already documented for the `By*`
maps (proxy/usage_tracker.go:826-827). The response is therefore "errors since
this field shipped" for multi-day periods, not "errors forever".

### 4B. Pool health — `pool/account.go` + `proxy/handler.go`

The live health state exists but is completely unreachable from HTTP.
`cooldowns` (pool/account.go:91), `errorCounts` (:92) and `modelLocks` (:95) are
unexported fields; no admin route reads them, and the predicate that would answer
"is model X locked on account Y" (`isModelLocked`, :993-1004) is unexported with
no external callers. The only pool-health value in the API is `/admin/api/status`'s
`"available": h.pool.AvailableCount()` (proxy/handler.go:11339), and
`AvailableCount` (:1516-1533) counts account-level cooldowns only — model locks
are invisible to it.

New accessor:

```go
type AccountHealth struct {
    CooldownUntil     time.Time
    CooldownReason    string
    ConsecutiveErrors int
    ModelLocks        map[string]ModelLock
}

type ModelLock struct {
    Until  time.Time
    Reason string
}
```

Four design constraints, each verified in source:

1. **Filter expired entries at read time with `now.Before(until)`; never prune.**
   Expired cooldowns and locks are never deleted — the only deletes in the file
   are in `RecordSuccess` (:1081, :1085, :1087) and `ClearCooldown` (:1459,
   :1460). A naive map dump reports long-dead cooldowns as active. Mirror the
   read-time filter `AvailableCount` already uses (:1524).
2. **Return `ConsecutiveErrors`.** An account one failure away from a lock
   (`recordErrorWithClass` trips at `>= 3`, :1127-1128) is currently
   indistinguishable from a healthy one.
3. **Persist the reason.** `CooldownClass` is computed by `ClassifyCooldown`
   (pool/cooldown_class.go:146) and returned only so the caller can log it
   (proxy/account_failover.go:287, :325, :343) — it is never stored. Without
   storing it the page can say "locked until 15:04" but never "because
   auth_failed", which is the single most useful thing it could say. Add
   `lockReasons map[string]string`, keyed `accountID` for account-level cooldowns
   and `accountID + "\x00" + model` for model locks, written in
   `recordErrorWithClass` (:1117-1142), cleared wherever the lock it describes is
   cleared: `RecordSuccess` (both keys) and `ClearCooldown` (all keys for the
   account).
4. **Do not call `config.*` while holding `p.mu`.** The lock order is documented
   at pool/account.go:1029-1030 and :322-326 ("Read config before taking p.mu …
   must not serialise against chat routing through cfgLock").

New route `GET /admin/api/pool/health`, ETag-wrapped like its neighbours,
returning the per-account map plus `since` and `uptimeSeconds`.

**Restart blindness, handled by disclosure rather than persistence.** All three
maps are created empty in `GetPool()` (:130-135) and nothing restores them;
`Reload()` reseeds only cumulative stats. A freshly restarted proxy reports every
vendor healthy, including one whose key was revoked a minute earlier. Persisting
cooldowns across restarts is a larger change with its own correctness questions,
so v1 instead labels the panel *"trạng thái từ lúc khởi động · X phút trước"* and
reads uptime from `/admin/api/status` (proxy/handler.go:11352). A health panel
that goes green on restart is worse than none, because the operator reads it as
evidence.

## 5. Frontend

### Files

| File | Change |
|---|---|
| `web-next/src/lib/providers.ts` | NEW — pure derivation only, no React and no fetch: `vendorKey(account)`, `groupByVendor(accounts, usage, health) → ProviderRow[]`, `recentErrors(usage, accountIndex, limit)`. Everything testable without rendering. |
| `web-next/src/components/ProvidersView.tsx` | NEW — props-in view. `PageHeader title="Nhà cung cấp"`, vendor card grid, period selector, recent-errors panel. Fetches nothing on mount. |
| `web-next/src/lib/providers.test.ts` | NEW |
| `web-next/src/components/ProvidersView.test.tsx` | NEW |
| `web-next/src/components/Shell.test.tsx` | NEW — the repo has no App or Shell test at all, so the registration edits below are otherwise covered only indirectly. |
| `web-next/src/components/Shell.tsx:3` | Add `'providers'` to the `Section` union. |
| `web-next/src/components/Shell.tsx:4-12` | Add one `items` entry, e.g. `{id:'providers',label:'Nhà cung cấp',hint:'Vendor, model & lỗi',icon:'⇄'}`. This is the only nav edit needed — the desktop sidebar (:18) and the mobile `<select>` (:23) both map over the same array. |
| `web-next/src/App.tsx:25` | Add `'providers'` to `VALID_SECTIONS`; without it `#providers` silently falls back to `overview` via `initialSection()` (:29-32). |
| `web-next/src/App.tsx` `load()` | One line: `if (section === 'providers') setUsage(await api.usage(usagePeriod))`. `loadCore()` already fetches accounts unconditionally, so accounts cost nothing extra. Add the health fetch here too. |
| `web-next/src/App.tsx:135-147` | One ternary arm rendering `<ProvidersView …/>`. |
| `web-next/src/lib/api.ts` | Add the `AccountHealth` interfaces and `api.poolHealth()`; add `errors?: number` to `PeriodSummary` (:56). |

**Deliberately excluded from the 15s auto-refresh** (App.tsx:124-130).
`/usage/stats` is documented in-source as the largest polled payload (~205 KiB);
polling it every 15s for a page of aggregates is waste. Manual "Làm mới" via
`onReload` is the refresh path. This is a decision, not an oversight.

### Conventions the view must follow

- One `Card className="p-5"` per vendor in `grid gap-3 sm:grid-cols-2 xl:grid-cols-4`
  (the QuotaView idiom).
- Reuse `accountLabel`, `providerLabel`, `health`, `errorRate`, `exactNumber`,
  `relativeTime` from `lib/format.ts` — do not reimplement.
- `const [busy,setBusy]=useState('')` driving a `Đang làm mới…` / `Làm mới` label
  swap (QuotaView idiom).
- `role="status"` for notices, `role="alert"` for errors (App.tsx:158-159).
- All strings hardcoded Vietnamese; there is no i18n layer in this client.
- **Do not use `.chip`, `.chip-on`, or `.btn-danger-soft`.** `web-next/src/index.css`
  defines only `.btn`, `.btn-primary`, `.input`; those three classes are referenced
  at AccountsView.tsx:32 and :42 and exist in no stylesheet, so those buttons
  currently render unstyled. Use inline Tailwind or `.btn`.
- `Section`, `KV` and `Label` are file-local to AccountsView.tsx:51-53 and not
  exported. Define local copies in ProvidersView; do **not** lift them into
  Shell.tsx as part of this change, which would widen the diff into AccountsView
  for no v1 benefit. When writing the local `KV`, use `v ?? '—'` rather than
  AccountsView's `String(v||'—')`, which renders a legitimate `0` as an em dash.

### Vendor key

Normalise `baseUrl` (strip scheme, strip trailing slashes) for external vendors;
fall back to `provider` / `providerKind` when it is empty. This mirrors the legacy
client's two-path grouping (web/accounts.js:112-116 for external, :381-395 for the
auth-method-derived category).

## 6. v1 scope

Ships:

1. Vendor list and grouping.
2. Per-vendor account roster — total plus enabled/banned/disabled breakdown using
   `health()`.
3. Per-vendor call counts, lifetime and windowed, with the period selector bound
   to `usagePeriod`.
4. Per-vendor error counts and rate — windowed from the new `errors` field, plus
   the cumulative figure explicitly labelled **luỹ kế**.
5. Recent-errors panel from `recentRequests`, newest first, attributed by
   `accountId` join. Header states the source is a 500-record buffer, not a window.
6. Per-vendor models, as the five catalog fields published for exactly this
   reason — `catalogState`, `catalogSource`, `catalogError`, `catalogCheckedAt`
   and `modelCount` (proxy/handler.go:7991-7994: "a service provider with no chat
   catalog, a provider whose credential just died, and one that was never probed
   all report zero").
7. Live health: cooldown deadline + reason, locked models + reason, consecutive
   error count — with the "since process start" disclosure.

Deferred: per-vendor model ID lists (needs an aggregate the API does not emit);
per-vendor quota rollup (the existing `QuotaOverview.providers` buckets by auth
family, not vendor); per-vendor last-error and latency; persisting cooldowns
across restarts; `#providers/<vendor>` sub-pages.

## 7. Testing

**Go**

| Test | Asserts |
|---|---|
| `TestPeriodSummaryCountsErrors` | A failed record increments `Errors` on the day bucket via `addToSummaryMap`. |
| `TestMergeSummaryCarriesErrors` | Folding two day buckets for a multi-day period preserves the error count. **This is the regression test for the two-merge bug** — without edit 3 it fails while 24h still passes. |
| `TestHealthSnapshotFiltersExpired` | A cooldown whose deadline has passed is absent from the snapshot; an active one is present. |
| `TestHealthSnapshotReportsReason` | A classified failure records its reason against the lock, and the reason is cleared by both `RecordSuccess` and `ClearCooldown`. |

**vitest**

| Test | Asserts |
|---|---|
| `providers.test.ts` | `vendorKey` strips scheme and trailing slashes and collapses two accounts on one host; falls back to `provider` when `baseUrl` is empty (a Kiro account must not land in an empty-string bucket); `groupByVendor` sums `byAccount[id].requests` for exactly the accounts in the group; an account absent from `byAccount` counts as 0, not `NaN`; an empty `byAccount` yields 0, not null; a fixture where two different hosts both report `provider: 'External OpenAI'` produces **two** vendor rows, not one. |
| `ProvidersView.test.tsx` | One card per vendor with the expected account count; **a vendor with 0 errors renders `"0"`, not `"—"`** (the falsy-trap regression); changing the period calls `onPeriod`; the recent-errors panel attributes a record whose `provider` field says `'External OpenAI'` to the vendor its `accountId` belongs to, not to the label; no rendered output or call payload contains a credential-shaped key (`accessToken`, `apiKey`, `extKeyMasked`). |
| `Shell.test.tsx` | `getByRole('button', {name:/Nhà cung cấp/})` exists and clicking it calls `onSection('providers')`. |

Fixtures are typed against the real exported interfaces so a shape change breaks
compilation. `VALID_SECTIONS` (App.tsx:25) stays untested by the above; the
`tsc -b` step in `npm run build` already fails on a `Section` union mismatch, so
the type-level half is guarded.

## 8. Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | `lockReasons` not cleared where its lock is cleared, so a stale reason is shown against a fresh lock | Clear at all three sites: `RecordSuccess` (account key + model key), `ClearCooldown` (all keys for the account). Covered by `TestHealthSnapshotReportsReason`. |
| 2 | `mergeSummaryMapInto` missed, so 7d/30d silently reports zero errors | Dedicated regression test; called out as edit 3 of §4A. |
| 3 | Cumulative `errorCount` labelled as consecutive failures | UI wording rule in §5; `errorCount` always carries the "luỹ kế" label. |
| 4 | Health panel reads green after a restart while accounts are genuinely locked | Uptime disclosure in the panel header; explicitly not solved by persistence in v1. |
| 5 | `/usage/stats` payload growth from the new field | `omitempty`, one int per account per day bucket. The payload is already ~205 KiB and ETag-wrapped. |

## 9. Security

- No new credential is read or emitted. The health endpoint returns timestamps,
  counters and class names only — never tokens or `baseUrl` credentials.
- `web-next` already authenticates with the same admin session token; the new
  route sits behind the same `adminSessions.valid()` gate as every other
  `/admin/api/` route (proxy/handler.go:6155-6160).
- The recent-errors panel renders upstream error strings, which can contain
  account emails. They are already returned today by `/admin/api/usage/stats` and
  rendered by the Usage view; no new exposure.
- A vitest assertion checks that no credential-shaped key reaches the DOM or a
  call payload.

## 10. Related files

**Modified (Go)**
- `proxy/usage_tracker.go` — `PeriodSummary`, `addToSummaryMap`, `mergeSummaryMapInto`
- `pool/account.go` — `AccountHealth`, `HealthSnapshot`, `lockReasons`, `recordErrorWithClass`, `RecordSuccess`, `ClearCooldown`
- `proxy/handler.go` — route registration + handler

**Modified (TS)**
- `web-next/src/components/Shell.tsx`, `web-next/src/App.tsx`, `web-next/src/lib/api.ts`

**Created**
- `web-next/src/lib/providers.ts`, `web-next/src/components/ProvidersView.tsx`
- `web-next/src/lib/providers.test.ts`, `web-next/src/components/ProvidersView.test.tsx`, `web-next/src/components/Shell.test.tsx`

**Not touched**
- `web/` (the legacy client) and `web-next/src/components/AccountsView.tsx`

## 11. Next steps

1. Write the implementation plan (phases, in this directory).
2. Backend first (§4A then §4B), each with its Go tests, since the frontend
   depends on both response shapes.
3. Frontend (§5), then `npm run build` in `web-next/` to refresh
   `webnext/dist`, which is what the Go binary embeds.
4. Restart the service and verify against live data.
