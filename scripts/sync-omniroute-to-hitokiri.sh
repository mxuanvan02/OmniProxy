#!/usr/bin/env bash
# sync-omniroute-to-hitokiri.sh — sync OmniRoute config (local → hitokiri)
#
# Exports provider connections + nodes from local OmniRoute (port 20128)
# and imports them on the hitokiri instance via the management API.
# Live apply — no restart, no DB file copy.
#
# Usage:
#   ./scripts/sync-omniroute-to-hitokiri.sh             # sync providers only
#   ./scripts/sync-omniroute-to-hitokiri.sh --full      # providers + combos + apiKeys + settings
#   ./scripts/sync-omniroute-to-hitokiri.sh --parts providers,combos
set -euo pipefail

REMOTE="${REMOTE:-hitokiri}"
LOCAL_PORT="${LOCAL_PORT:-20128}"
REMOTE_PORT="${REMOTE_PORT:-20128}"
ADMIN_PASS="${ADMIN_PASS:-CHANGEME}"
MODE="providers"
[[ "${1:-}" == "--full" ]] && MODE="full"
[[ "${1:-}" == "--parts" && -n "${2:-}" ]] && MODE="$2"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl required"
command -v jq   >/dev/null 2>&1 || fail "jq required"
command -v ssh  >/dev/null 2>&1 || fail "ssh required"

# ── 1. Login local ────────────────────────────────────────────────────
log "Authenticating to local OmniRoute (:${LOCAL_PORT})..."
LOCAL_TOKEN="$(curl -s -i -X POST "http://localhost:${LOCAL_PORT}/api/auth/login" \
  -H "Content-Type: application/json" -d "{\"password\":\"${ADMIN_PASS}\"}" \
  | grep -i "set-cookie:" | sed 's/.*auth_token=//;s/;.*//')"
[[ -n "$LOCAL_TOKEN" ]] || fail "Local login failed (wrong ADMIN_PASS?)"

# ── 2. Export config ──────────────────────────────────────────────────
log "Exporting config from local (mode: ${MODE})..."
EXPORT_JSON="$(curl -s "http://localhost:${LOCAL_PORT}/api/settings/export-json" \
  -H "Cookie: auth_token=${LOCAL_TOKEN}")"
[[ -n "$EXPORT_JSON" && "$EXPORT_JSON" != "null" ]] || fail "Export failed"

# Build partial payload based on mode
case "$MODE" in
  providers) PARTS='["providerConnections","providerNodes"]' ;;
  full)      PARTS='["providerConnections","providerNodes","combos","apiKeys","settings"]' ;;
  *)         PARTS="$(echo "$MODE" | jq -R 'split(",") | map(gsub("^\\s+|\\s+$";""))')" ;;
esac

PAYLOAD="$(echo "$EXPORT_JSON" | jq -c --argjson parts "$PARTS" 'with_entries(select(.key as $k | $parts | index($k)))')"
[[ "$PAYLOAD" != "{}" ]] || fail "No matching parts in export"

part_summary="$(echo "$PAYLOAD" | jq -r 'to_entries | map("\(.key)=\(.value | length)") | join(", ")')"
log "  payload: ${part_summary}"

# ── 3. Login remote + import ──────────────────────────────────────────
log "Authenticating to ${REMOTE} OmniRoute (:${REMOTE_PORT})..."
REMOTE_TOKEN="$(ssh "$REMOTE" "curl -s -i -X POST http://localhost:${REMOTE_PORT}/api/auth/login \
  -H 'Content-Type: application/json' -d '{\"password\":\"${ADMIN_PASS}\"}'" \
  | grep -i "set-cookie:" | sed 's/.*auth_token=//;s/;.*//')"
[[ -n "$REMOTE_TOKEN" ]] || fail "Remote login failed"

log "Importing config on ${REMOTE}..."
RESULT="$(echo "$PAYLOAD" | ssh "$REMOTE" "curl -s -X POST http://localhost:${REMOTE_PORT}/api/settings/import-json \
  -H 'Cookie: auth_token=${REMOTE_TOKEN}' \
  -H 'Content-Type: application/json' \
  --data-binary @-")"
echo "$RESULT" | jq -c '{success, connections: .connections, nodes: .nodes, combos: .combos, apiKeys: .apiKeys, message}' 2>/dev/null || echo "$RESULT"

log "Done."
