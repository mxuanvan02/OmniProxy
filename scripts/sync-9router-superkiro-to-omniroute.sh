#!/usr/bin/env bash
# sync-9router-superkiro-to-omniroute.sh — import missing accounts from legacy 9router + SuperKiro
#
# Reads ~/.9router/db.json and SuperKiro/data/config.json, merges with current
# OmniRoute provider connections + nodes, and imports the full merged set so
# nothing is lost (import-json may replace, so we send everything).
#
# Usage:
#   ./scripts/sync-9router-superkiro-to-omniroute.sh            # apply
#   ./scripts/sync-9router-superkiro-to-omniroute.sh --dry-run  # preview only
set -euo pipefail

OMNI_PORT="${OMNI_PORT:-20128}"
ADMIN_PASS="${ADMIN_PASS:-CHANGEME}"
NINEROUTER_DB="${NINEROUTER_DB:-$HOME/.9router/db.json}"
SUPERKIRO_CFG="${SUPERKIRO_CFG:-$HOME/ProxyKiro/SuperKiro/data/config.json}"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" || "${1:-}" == "-n" ]] && DRY_RUN=1

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl required"
command -v jq   >/dev/null 2>&1 || fail "jq required"
[[ -f "$NINEROUTER_DB" ]] || fail "9router db not found: $NINEROUTER_DB"
[[ -f "$SUPERKIRO_CFG" ]] || fail "SuperKiro config not found: $SUPERKIRO_CFG"

# ── 1. Login local OmniRoute ──────────────────────────────────────────
log "Authenticating to OmniRoute (:${OMNI_PORT})..."
TOKEN="$(curl -s -i -X POST "http://localhost:${OMNI_PORT}/api/auth/login" \
  -H "Content-Type: application/json" -d "{\"password\":\"${ADMIN_PASS}\"}" \
  | grep -i "set-cookie:" | sed 's/.*auth_token=//;s/;.*//')"
[[ -n "$TOKEN" ]] || fail "Login failed"

# ── 2. Export current OmniRoute ───────────────────────────────────────
log "Exporting current OmniRoute config..."
EXPORT="$(curl -s "http://localhost:${OMNI_PORT}/api/settings/export-json" \
  -H "Cookie: auth_token=${TOKEN}")"
[[ -n "$EXPORT" && "$EXPORT" != "null" ]] || fail "Export failed"

# ── 3. Merge with 9router + SuperKiro ─────────────────────────────────
log "Merging with 9router + SuperKiro..."
MERGED="$(jq -n --argjson omni "$EXPORT" --slurpfile r9 <(jq '{providerConnections, providerNodes}' "$NINEROUTER_DB") --slurpfile sk <(jq '{accounts}' "$SUPERKIRO_CFG") '
  ($omni.providerConnections // []) as $omni_c |
  ($omni.providerNodes // []) as $omni_n |
  ($r9[0].providerConnections // []) as $r9_c |
  ($r9[0].providerNodes // []) as $r9_n |
  ($sk[0].accounts // []) as $sk_accs |

  # OmniRoute connection names (dedupe key)
  ($omni_c | map(.name)) as $omni_names |
  ($omni_n | map(.id)) as $omni_node_ids |

  # 9router connections missing in OmniRoute
  ($r9_c | map(select(.name as $n | $omni_names | index($n) | not))) as $r9_new |

  # 9router nodes missing in OmniRoute
  ($r9_n | map(select(.id as $id | $omni_node_ids | index($id) | not))) as $r9_new_nodes |

  # SuperKiro codex accounts -> OmniRoute codex connection format (only missing)
  ($sk_accs | map(
    select(.enabled == true or .enabled == "True" or .enabled == "true" or .enabled == 1) |
    select(.email as $e | $omni_names | index($e) | not) |
    {
      id: .id,
      provider: "codex",
      authType: "oauth",
      name: .email,
      email: .email,
      isActive: 1,
      accessToken: .accessToken,
      refreshToken: .refreshToken,
      expiresAt: (.expiresAt | tonumber? // .),
      providerSpecificData: {
        chatgptAccountId: .chatgptAccountId,
        machineId: .machineId,
        region: .region,
        nickname: .nickname
      },
      createdAt: (now | todateiso8601),
      updatedAt: (now | todateiso8601)
    }
  )) as $sk_new |

  {
    providerConnections: ($omni_c + $r9_new + $sk_new),
    providerNodes: ($omni_n + $r9_new_nodes),
    _meta: {
      source: "merge-9router-superkiro",
      omniExisting: ($omni_c | length),
      r9Added: ($r9_new | length),
      skAdded: ($sk_new | length),
      nodesAdded: ($r9_new_nodes | length),
      total: (($omni_c | length) + ($r9_new | length) + ($sk_new | length))
    }
  }
')"

SUMMARY="$(echo "$MERGED" | jq -r '._meta | "omni=\(.omniExisting) + 9router=\(.r9Added) + superkiro=\(.skAdded) (nodes +\(.nodesAdded)) = total \(.total)"')"
log "  $SUMMARY"

if [[ $DRY_RUN -eq 1 ]]; then
  log "Dry-run: no changes applied."
  echo "$MERGED" | jq 'del(._meta) | {providerConnections: (.providerConnections | length), providerNodes: (.providerNodes | length)}'
  exit 0
fi

# ── 4. Import merged ──────────────────────────────────────────────────
log "Importing merged config to OmniRoute..."
RESULT="$(echo "$MERGED" | jq 'del(._meta)' | curl -s -X POST "http://localhost:${OMNI_PORT}/api/settings/import-json" \
  -H "Cookie: auth_token=${TOKEN}" \
  -H "Content-Type: application/json" \
  --data-binary @-)"
echo "$RESULT" | jq -c '{success, connections, nodes, message}' 2>/dev/null || echo "$RESULT"
log "Done."
