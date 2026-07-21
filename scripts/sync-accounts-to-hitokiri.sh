#!/usr/bin/env bash
# sync-accounts-to-hitokiri.sh — fast account state sync (local → hitokiri)
#
# Syncs ONLY account enabled/disabled state via the admin API.
# No binary, no web, no full config merge, NO restart — the proxy's
# /admin/api/accounts/batch endpoint applies the change in-memory,
# persists to config.json, and reloads the pool live.
#
# Use this when you toggled accounts locally and want the same
# enabled/disabled set on hitokiri within seconds.
#
# Usage:
#   ./scripts/sync-accounts-to-hitokiri.sh           # sync + show diff
#   ./scripts/sync-accounts-to-hitokiri.sh --dry-run  # show diff, no changes
set -euo pipefail

# ── Config ───────────────────────────────────────────────────────────
REMOTE="${REMOTE:-hitokiri}"
REMOTE_PORT="${REMOTE_PORT:-20131}"
LOCAL_PORT="${LOCAL_PORT:-8080}"
ADMIN_PASS="${ADMIN_PASS:-changeme}"
SERVICE_NAME="${SERVICE_NAME:-superkiro-user.service}"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" || "${1:-}" == "-n" ]] && DRY_RUN=1

# ── Helpers ──────────────────────────────────────────────────────────
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"

# ── Fetch account state from both proxies ────────────────────────────
log "Fetching local accounts (port ${LOCAL_PORT})..."
local_json="$(curl -s --max-time 10 \
  -H "X-Admin-Password: ${ADMIN_PASS}" \
  "http://localhost:${LOCAL_PORT}/admin/api/accounts")"
[[ -n "$local_json" && "$local_json" != "null" ]] || fail "No local accounts response (is proxy on :${LOCAL_PORT} running?)"

log "Fetching remote accounts (${REMOTE}:${REMOTE_PORT})..."
remote_json="$(ssh "$REMOTE" "curl -s --max-time 10 \
  -H 'X-Admin-Password: ${ADMIN_PASS}' \
  'http://localhost:${REMOTE_PORT}/admin/api/accounts'")"
[[ -n "$remote_json" && "$remote_json" != "null" ]] || fail "No remote accounts response (is proxy on ${REMOTE}:${REMOTE_PORT} running?)"

local_count="$(echo "$local_json" | jq 'length')"
remote_count="$(echo "$remote_json" | jq 'length')"
log "  local:  ${local_count} accounts"
log "  remote: ${remote_count} accounts"

# ── Compute diff: which IDs to enable / disable on remote ────────────
# Match by account ID (stable across renames). For each local account:
#   - if local.enabled == true  and remote.enabled == false → enable on remote
#   - if local.enabled == false and remote.enabled == true  → disable on remote
# Accounts that exist locally but not remotely are reported as missing
# (the full sync-omniproxy-to-hitokiri.sh --config handles adding them).
diff_json="$(jq -s '
  .[0] as $local |
  .[1] as $remote |
  ($remote | map({(.id): .}) | add // {}) as $remote_by_id |
  ($local  | map({(.id): .}) | add // {}) as $local_by_id  |
  {
    to_enable:  [
      $local[] |
      select(.enabled == true) as $l |
      ($remote_by_id[$l.id] // {enabled: null}) as $r |
      select($r.enabled == false) |
      {id: $l.id, email: $l.email}
    ],
    to_disable: [
      $local[] |
      select(.enabled == false) as $l |
      ($remote_by_id[$l.id] // {enabled: null}) as $r |
      select($r.enabled == true) |
      {id: $l.id, email: $l.email}
    ],
    missing_on_remote: [
      $local[] |
      select(.id as $id | $remote_by_id | has($id) | not) |
      {id: .id, email: .email, enabled: .enabled}
    ],
    missing_on_local: [
      $remote[] |
      select(.id as $id | $local_by_id | has($id) | not) |
      {id: .id, email: .email, enabled: .enabled}
    ]
  }
' <(echo "$local_json") <(echo "$remote_json"))"

enable_count="$(echo "$diff_json" | jq '.to_enable | length')"
disable_count="$(echo "$diff_json" | jq '.to_disable | length')"
missing_remote_count="$(echo "$diff_json" | jq '.missing_on_remote | length')"
missing_local_count="$(echo "$diff_json" | jq '.missing_on_local | length')"

echo
echo "── Diff (local → ${REMOTE}) ──"
echo "  to enable:        ${enable_count}"
echo "  to disable:       ${disable_count}"
echo "  missing on remote:${missing_remote_count}  (use full sync --config to add)"
echo "  missing on local: ${missing_local_count}   (use full sync --config to remove)"

if [[ "$enable_count" -gt 0 ]]; then
  echo "  ─ enable list:"
  echo "$diff_json" | jq -r '.to_enable[] | "    + \(.email) (\(.id))"'
fi
if [[ "$disable_count" -gt 0 ]]; then
  echo "  ─ disable list:"
  echo "$diff_json" | jq -r '.to_disable[] | "    - \(.email) (\(.id))"'
fi
if [[ "$missing_remote_count" -gt 0 ]]; then
  echo "  ─ missing on remote (NOT synced — run full sync):"
  echo "$diff_json" | jq -r '.missing_on_remote[] | "    ? \(.email) (\(.id)) enabled=\(.enabled)"'
fi

# ── Nothing to do? ───────────────────────────────────────────────────
if [[ "$enable_count" -eq 0 && "$disable_count" -eq 0 ]]; then
  echo
  log "Already in sync ✓ (enabled/disabled state matches local)"
  exit 0
fi

# ── Dry run stops here ───────────────────────────────────────────────
if [[ "$DRY_RUN" -eq 1 ]]; then
  echo
  log "Dry run — no changes applied. Re-run without --dry-run to sync."
  exit 0
fi

# ── Apply via /admin/api/accounts/batch (no restart needed) ──────────
apply_batch() {
  local action="$1" ids_json="$2" count="$3"
  if [[ "$count" -eq 0 ]]; then return; fi
  log "Applying ${action} to ${count} account(s) on ${REMOTE}..."
  local payload
  payload="$(jq -nc --argjson ids "$ids_json" --arg action "$action" \
    '{ids: $ids, action: $action}')"
  local resp
  resp="$(ssh "$REMOTE" "curl -s --max-time 15 \
    -H 'X-Admin-Password: ${ADMIN_PASS}' \
    -H 'Content-Type: application/json' \
    -X POST \
    -d '${payload}' \
    'http://localhost:${REMOTE_PORT}/admin/api/accounts/batch'")"
  local ok
  ok="$(echo "$resp" | jq -r '.success // false')"
  local applied
  applied="$(echo "$resp" | jq -r '.count // 0')"
  if [[ "$ok" == "true" ]]; then
    log "  ✓ ${action}: ${applied} account(s) updated (pool reloaded live, no restart)"
  else
    log "  ✗ ${action} failed: ${resp}"
    return 1
  fi
}

enable_ids="$(echo "$diff_json" | jq -c '[.to_enable[].id]')"
disable_ids="$(echo "$diff_json" | jq -c '[.to_disable[].id]')"

apply_batch "enable"  "$enable_ids"  "$enable_count"  || true
apply_batch "disable" "$disable_ids" "$disable_count" || true

# ── Verify ───────────────────────────────────────────────────────────
echo
log "Verifying..."
sleep 1
verify_json="$(ssh "$REMOTE" "curl -s --max-time 10 \
  -H 'X-Admin-Password: ${ADMIN_PASS}' \
  'http://localhost:${REMOTE_PORT}/admin/api/accounts'")"

mismatch="$(jq -s '
  .[0] as $local |
  .[1] as $remote |
  ($remote | map({(.id): .enabled}) | add // {}) as $remote_state |
  [$local[] |
   select(.id as $id | $remote_state | has($id)) |
   select(.enabled != ($remote_state[.id] // null)) |
   "\(.email): local=\(.enabled) remote=\($remote_state[.id] // "?")"
  ] | length
' <(echo "$local_json") <(echo "$verify_json"))"

if [[ "$mismatch" -eq 0 ]]; then
  log "Sync complete ✓ — all account enabled/disabled states match local"
else
  log "Warning: ${mismatch} account(s) still mismatched — check remote logs"
  exit 1
fi
