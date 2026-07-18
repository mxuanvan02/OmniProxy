#!/usr/bin/env bash
set -euo pipefail

REMOTE="${REMOTE:-hitokiri}"
REMOTE_CONFIG="${REMOTE_CONFIG:-/home/hitokiri/.superkiro-user/data/config.json}"
LOCAL_CONFIG="${LOCAL_CONFIG:-/Users/van/ProxyKiro/SuperKiro/data/config.json}"
REMOTE_TMP="/tmp/superkiro-local-config.$$.json"

if [[ ! -f "$LOCAL_CONFIG" ]]; then
  echo "Local SuperKiro config not found: $LOCAL_CONFIG" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required locally." >&2
  exit 1
fi

local_accounts="$(jq '.accounts | length' "$LOCAL_CONFIG")"
local_keys="$(jq '.apiKeys | length' "$LOCAL_CONFIG")"
local_email="$(jq -r '.accounts[0].email // "unknown"' "$LOCAL_CONFIG")"

if [[ "$local_accounts" -lt 1 ]]; then
  echo "Local config has no accounts; refusing to sync." >&2
  exit 1
fi

echo "Syncing ${local_accounts} SuperKiro account(s), ${local_keys} API key(s) to ${REMOTE}"
echo "Primary local account: ${local_email}"

scp "$LOCAL_CONFIG" "${REMOTE}:${REMOTE_TMP}"

ssh "$REMOTE" "REMOTE_CONFIG='${REMOTE_CONFIG}' REMOTE_TMP='${REMOTE_TMP}' bash -s" <<'REMOTE_SCRIPT'
set -euo pipefail

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required on hitokiri." >&2
  exit 1
fi

if [[ ! -f "$REMOTE_CONFIG" ]]; then
  echo "Remote SuperKiro config not found: $REMOTE_CONFIG" >&2
  rm -f "$REMOTE_TMP"
  exit 1
fi

stamp="$(date +%Y%m%d-%H%M%S)"
backup="${REMOTE_CONFIG}.bak-sync-local-${stamp}"
merged="/tmp/superkiro-merged-config.${stamp}.json"

cp "$REMOTE_CONFIG" "$backup"
chmod 600 "$backup" 2>/dev/null || true

jq -s '
  .[0] as $remote |
  .[1] as $local |
  $remote
  | .accounts = ($local.accounts // [])
  | .apiKeys = ($local.apiKeys // $remote.apiKeys // [])
' "$REMOTE_CONFIG" "$REMOTE_TMP" > "$merged"

install -m 600 "$merged" "$REMOTE_CONFIG"
rm -f "$REMOTE_TMP" "$merged"

systemctl --user restart superkiro-user.service
sleep 2

echo "Backup: $backup"
systemctl --user --no-pager --full status superkiro-user.service | sed -n '1,12p'
echo
echo "Models endpoint:"
curl -sS http://localhost:20131/v1/models | jq '{object, data_len:(.data|length), first:(.data[0].id // null)}'

echo
echo "Messages smoke test:"
curl -sS http://localhost:20131/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4.5","max_tokens":16,"messages":[{"role":"user","content":"Tra loi dung mot tu: OK"}]}' \
  | jq '{id, type, model, text:(.content[0].text // .error.message // .message // null)}'
REMOTE_SCRIPT

