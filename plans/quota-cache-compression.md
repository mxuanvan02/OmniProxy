# OmniProxy Enhancement Plan: Quota Tracker + Cache + Compression

## Goal
Enhance OmniProxy with dedicated Quota Tracker page, full provider-native cache (90-99%), cache token viewing, and compression features learned from OmniRoute/9router.

## Phase 1: Backend — Cache tracking & passthrough
- [x] cache_tracker.go already tracks Claude cache_creation/read per account
- [ ] Add cache fields to RequestRecord + PeriodSummary (cacheRead, cacheCreation, cacheSaved)
- [ ] Add `cached_tokens` passthrough for OpenAI/Codex (prompt_tokens_details)
- [ ] Add `/admin/api/cache/stats` endpoint — aggregate cache stats per account/model
- [ ] Track cache savings in usage_tracker

## Phase 2: Backend — Quota Tracker API
- [ ] Add `/admin/api/quota/overview` — aggregate quota by provider (Kiro, Codex, External)
- [ ] Add `/admin/api/quota/accounts` — per-account quota breakdown with reset dates

## Phase 3: Frontend — Quota Tracker page
- [ ] Add new "Quota" tab in index.html
- [ ] Create quota.js — quota tracker page:
  - Provider summary cards (Kiro, Codex, External) with aggregate usage %
  - Per-account quota bars with reset dates
  - Quota utilization timeline

## Phase 4: Frontend — Cache token viewing
- [ ] Add "Cache" sub-tab in Usage page
- [ ] Show cache hit/miss ratio, tokens saved, cache efficiency %
- [ ] Per-account/model cache breakdown

## Phase 5: Compression (learn from 9router/OmniRoute)
- [ ] Add RTK-style tool output compression (compress git/grep/ls output)
- [ ] Add optional Headroom integration (proxy to headroom for prompt compression)
- [ ] Add Caveman-style terse output system prompt injection

## Phase 6: Cleanup
- [ ] Remove OmniRoute artifacts
- [ ] Verify build + tests pass
