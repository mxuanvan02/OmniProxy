#!/usr/bin/env bash
# sync-omniproxy-to-hitokiri.sh — full sync of OmniProxy to hitokiri
#
# Syncs: binary + web assets + config (merged, not overwritten)
# Target: hitokiri server, running superkiro-user.service on port 20131
#
# Usage:
#   ./scripts/sync-omniproxy-to-hitokiri.sh           # sync everything
#   ./scripts/sync-omniproxy-to-hitokiri.sh --config   # config only
#   ./scripts/sync-omniproxy-to-hitokiri.sh --binary   # binary + web only
set -euo pipefail

# ── Config ───────────────────────────────────────────────────────────
REMOTE="${REMOTE:-hitokiri}"
REMOTE_HOME="${REMOTE_HOME:-/home/hitokiri/.superkiro-user}"
REMOTE_BIN="${REMOTE_BIN:-${REMOTE_HOME}/bin/superkiro}"
REMOTE_WEB="${REMOTE_WEB:-${REMOTE_HOME}/bin/web}"
REMOTE_CONFIG="${REMOTE_CONFIG:-${REMOTE_HOME}/data/config.json}"
LOCAL_CONFIG="${LOCAL_CONFIG:-/Users/van/ProxyKiro/SuperKiro/data/config.json}"
LOCAL_WEB="${LOCAL_WEB:-/Users/van/ProxyKiro/SuperKiro/web}"
LINUX_BIN="${LINUX_BIN:-/tmp/omniproxy-linux}"
SERVICE_NAME="${SERVICE_NAME:-superkiro-user.service}"
PORT="${PORT:-20131}"

# ── Args ─────────────────────────────────────────────────────────────
MODE="all"
case "${1:-}" in
  --config) MODE="config" ;;
  --binary) MODE="binary" ;;
  --all|*)  MODE="all" ;;
esac

# ── Helpers ──────────────────────────────────────────────────────────
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

# ── Pre-flight ───────────────────────────────────────────────────────
[[ -f "$LOCAL_CONFIG" ]] || fail "Local config not found: $LOCAL_CONFIG"
command -v jq >/dev/null 2>&1 || fail "jq is required locally"

# ── Build Linux binary (if syncing binary) ───────────────────────────
if [[ "$MODE" == "all" || "$MODE" == "binary" ]]; then
  log "Cross-compiling Linux amd64 binary..."
  cd "$(dirname "$0")/.."
  GOOS=linux GOARCH=amd64 go build -o "$LINUX_BIN" . || fail "Build failed"
  log "Binary built: $(ls -la "$LINUX_BIN" | awk '{print $5}') bytes"
fi

# ── Config stats ─────────────────────────────────────────────────────
local_accounts="$(jq '.accounts | length' "$LOCAL_CONFIG")"
local_keys="$(jq '.apiKeys | length' "$LOCAL_CONFIG")"
local_email="$(jq -r '.accounts[0].email // "unknown"' "$LOCAL_CONFIG")"
[[ "$local_accounts" -ge 1 ]] || fail "Local config has no accounts; refusing to sync."

log "Syncing to ${REMOTE} (mode: ${MODE})"
log "  Accounts: ${local_accounts}, API keys: ${local_keys}"
log "  Primary:  ${local_email}"

# ── Sync binary + web ────────────────────────────────────────────────
if [[ "$MODE" == "all" || "$MODE" == "binary" ]]; then
  log "Uploading binary → ${REMOTE}:${REMOTE_BIN}"
  ssh "$REMOTE" "mkdir -p '$(dirname "$REMOTE_BIN")'"
  scp -q "$LINUX_BIN" "${REMOTE}:${REMOTE_BIN}.new"
  ssh "$REMOTE" "chmod +x '${REMOTE_BIN}.new'"

  log "Uploading web assets → ${REMOTE}:${REMOTE_WEB}"
  ssh "$REMOTE" "mkdir -p '${REMOTE_WEB}.new'"
  scp -q -r "$LOCAL_WEB"/* "${REMOTE}:${REMOTE_WEB}.new/"

  # Atomic swap: backup old, move new into place
  ssh "$REMOTE" "REMOTE_BIN='${REMOTE_BIN}' REMOTE_WEB='${REMOTE_WEB}' bash -s" <<'SWAP'
set -euo pipefail
stamp="$(date +%Y%m%d-%H%M%S)"
bin="${REMOTE_BIN}"
web="${REMOTE_WEB}"

# Backup current binary
if [[ -f "$bin" ]]; then
  cp "$bin" "${bin}.bak-sync-${stamp}"
  chmod 600 "${bin}.bak-sync-${stamp}" 2>/dev/null || true
fi

# Backup current web dir
if [[ -d "$web" ]]; then
  tar czf "${web}.bak-sync-${stamp}.tgz" -C "$(dirname "$web")" "$(basename "$web")" 2>/dev/null || true
fi

# Swap binary
mv "${bin}.new" "$bin"
chmod +x "$bin"

# Swap web dir
rm -rf "${web}.old-sync"
if [[ -d "$web" ]]; then mv "$web" "${web}.old-sync"; fi
mv "${web}.new" "$web"
rm -rf "${web}.old-sync"

echo "Swap complete. Backups: ${bin}.bak-sync-${stamp}, ${web}.bak-sync-${stamp}.tgz"
SWAP
fi

# ── Sync config (merge, don't overwrite) ────────────────────────────
if [[ "$MODE" == "all" || "$MODE" == "config" ]]; then
  REMOTE_TMP="/tmp/omniproxy-config.$$.json"
  log "Uploading config → ${REMOTE} (will merge with remote)"
  scp -q "$LOCAL_CONFIG" "${REMOTE}:${REMOTE_TMP}"

  ssh "$REMOTE" "REMOTE_CONFIG='${REMOTE_CONFIG}' REMOTE_TMP='${REMOTE_TMP}' SERVICE_NAME='${SERVICE_NAME}' PORT='${PORT}' bash -s" <<'REMOTE_SCRIPT'
set -euo pipefail

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required on remote." >&2
  exit 1
fi

REMOTE_CONFIG="${REMOTE_CONFIG}"
REMOTE_TMP="${REMOTE_TMP}"
SERVICE_NAME="${SERVICE_NAME}"
PORT="${PORT}"

if [[ ! -f "$REMOTE_CONFIG" ]]; then
  echo "Remote config not found: $REMOTE_CONFIG" >&2
  rm -f "$REMOTE_TMP"
  exit 1
fi

stamp="$(date +%Y%m%d-%H%M%S)"
backup="${REMOTE_CONFIG}.bak-sync-${stamp}"
merged="/tmp/omniproxy-merged-${stamp}.json"

# Stop the service before touching the config — the running proxy holds config
# state in memory and will overwrite file changes (dropping newly-added accounts)
# if it stays up during install. The RESTART section at the end will start it again.
echo "Stopping ${SERVICE_NAME} before config install (avoid in-memory overwrite)..."
systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true

# Wait until the proxy process is actually gone — a still-alive proxy holds config
# state in memory and will overwrite the file (dropping newly-added accounts) when it
# eventually exits. systemctl stop returns before the process fully terminates.
for i in $(seq 1 20); do
  if ! pgrep -f "superkiro --no-menu" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
# Final hard check
if pgrep -f "superkiro --no-menu" >/dev/null 2>&1; then
  echo "WARN: proxy still running after 10s; killing to avoid config overwrite"
  pkill -f "superkiro --no-menu" 2>/dev/null || true
  sleep 1
fi

cp "$REMOTE_CONFIG" "$backup"
chmod 600 "$backup" 2>/dev/null || true

# Merge: local is source of truth for account existence + enabled + credentials.
# Remote runtime stats (requestCount, lastUsed, totalTokens, codexUsage*) are preserved.
# Accounts in remote but not local are dropped (deletion propagation).
# Accounts disabled in local become disabled in remote (disable propagation).
jq -s '
  .[0] as $remote |
  .[1] as $local |
  ($local.accounts // []) as $la |
  ($remote.accounts // []) as $ra |
  $remote
  | .accounts = (
      $la | map(
        . as $lacc |
        ($ra | map(select(.id == $lacc.id)) | first) as $racc |
        if $racc then
          $lacc * ($racc | {
            requestCount, lastUsed, totalTokens,
            codexPrimaryUsedPercent, codexPrimaryWindowMinutes,
            codexPrimaryResetAt, codexUsageCheckedAt
          } | with_entries(select(.value != null)))
        else
          $lacc
        end
      )
    )
  | .apiKeys = ($local.apiKeys // $remote.apiKeys // [])
' "$REMOTE_CONFIG" "$REMOTE_TMP" > "$merged"

# Diff summary: added / removed / disabled-changed (jq < 1.7 compatible — no set())
jq -s '
  .[0] as $remote |
  .[1] as $local |
  ($remote.accounts // []) as $ra |
  ($local.accounts // []) as $la |
  ($la | map({(.id): true}) | add // {}) as $local_ids |
  ($ra | map({(.id): true}) | add // {}) as $remote_ids |
  {
    added:   [$la[] | select(.id as $id | $remote_ids | has($id) | not) | .email],
    removed: [$ra[] | select(.id as $id | $local_ids  | has($id) | not) | .email],
    disabled_now: [($la[] | select(.enabled == false) as $lacc |
                    $ra[] | select(.id == $lacc.id and .enabled == true) |
                    "\($lacc.email) (was enabled)")],
    enabled_now:  [($la[] | select(.enabled == true) as $lacc |
                    $ra[] | select(.id == $lacc.id and .enabled == false) |
                    "\($lacc.email) (was disabled)")]
  }
' "$REMOTE_CONFIG" "$REMOTE_TMP" > /tmp/omniproxy-diff-$$.json

echo "Config merged. Backup: $backup"
echo "── Diff (local → remote) ──"
jq -r '
  "  added:        \(.added | length)   \(.added | map("  - " + .) | join("\n"))",
  "  removed:      \(.removed | length)   \(.removed | map("  - " + .) | join("\n"))",
  "  disabled_now: \(.disabled_now | length)   \(.disabled_now | map("  - " + .) | join("\n"))",
  "  enabled_now:  \(.enabled_now | length)   \(.enabled_now | map("  - " + .) | join("\n"))"
' /tmp/omniproxy-diff-$$.json
rm -f /tmp/omniproxy-diff-$$.json
REMOTE_SCRIPT
fi

# ── Restart service + smoke test ─────────────────────────────────────
log "Restarting ${SERVICE_NAME} on ${REMOTE}..."
ssh "$REMOTE" "SERVICE_NAME='${SERVICE_NAME}' PORT='${PORT}' bash -s" <<'RESTART'
set -euo pipefail
SERVICE_NAME="${SERVICE_NAME}"
PORT="${PORT}"

systemctl --user restart "$SERVICE_NAME"
sleep 3

echo "── Service status ──"
systemctl --user --no-pager --full status "$SERVICE_NAME" | sed -n '1,10p'

echo
echo "── Models endpoint ──"
curl -sS "http://localhost:${PORT}/v1/models" | jq '{object, data_len:(.data|length), first:(.data[0].id // null), last:(.data[-1].id // null)}'

echo
echo "── Chat smoke test (gpt-5.6-sol — Codex account) ──"
curl -sS "http://localhost:${PORT}/v1/chat/completions" \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-5.6-sol","max_tokens":16,"messages":[{"role":"user","content":"Tra loi dung mot tu: OK"}]}' \
  | jq '{model, text:(.choices[0].message.content // .error.message // null)}'
RESTART

log "Sync complete ✓"
