#!/usr/bin/env bash
# update.sh — update OmniProxy to the latest GitHub Release, fast and safe.
#
#   ./scripts/update.sh --check    # compare installed vs latest release, change nothing
#   ./scripts/update.sh            # download + verify sha256 → backup → swap → restart → health-check
#
# What it does:
#   1. Asks the GitHub API for the latest release tag of $OMNIPROXY_REPO.
#   2. Compares it with the installed version ($OMNIPROXY_HOME/version.json).
#   3. Downloads the tarball for this OS/arch plus its .sha256 and VERIFIES the
#      checksum before touching anything.
#   4. Backs the current binary up under $OMNIPROXY_HOME/.rollback/.
#   5. Swaps in the new binary + web/ + version.json.
#   6. Restarts the service (systemd --user by default; override with
#      $OMNIPROXY_RESTART_CMD, e.g. launchctl on macOS).
#   7. Health-checks /v1/models; on failure it restores the backup, restarts
#      again and exits non-zero. The previous binary is never deleted.
#
# Env overrides:
#   OMNIPROXY_REPO         default mxuanvan02/OmniProxy
#   OMNIPROXY_HOME         default ~/.omniproxy-user   (bin/omniproxy, bin/web, version.json)
#   OMNIPROXY_SERVICE      default omniproxy-user.service (systemd --user)
#   OMNIPROXY_PORT         default 8080                (health-check port)
#   OMNIPROXY_HEALTH_URL   default http://127.0.0.1:$OMNIPROXY_PORT/v1/models
#   OMNIPROXY_RESTART_CMD  custom restart command (skips systemd entirely)
#   OMNIPROXY_KEEP_ROLLBACKS  default 5 (older .rollback binaries are pruned)
#   OMNIPROXY_API_URL      override the release-API URL (testing/mirrors)

set -euo pipefail

REPO="${OMNIPROXY_REPO:-mxuanvan02/OmniProxy}"
HOME_DIR="${OMNIPROXY_HOME:-$HOME/.omniproxy-user}"
BIN="${HOME_DIR}/bin/omniproxy"
WEB="${HOME_DIR}/bin/web"
SERVICE="${OMNIPROXY_SERVICE:-omniproxy-user.service}"
PORT="${OMNIPROXY_PORT:-8080}"
HEALTH_URL="${OMNIPROXY_HEALTH_URL:-http://127.0.0.1:${PORT}/v1/models}"
RESTART_CMD="${OMNIPROXY_RESTART_CMD:-}"
KEEP_ROLLBACKS="${OMNIPROXY_KEEP_ROLLBACKS:-5}"
API="${OMNIPROXY_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"

log()  { echo "[update] $*"; }
fail() { echo "[update] ERROR: $*" >&2; exit 1; }

MODE="update"
case "${1:-}" in
  --check) MODE="check" ;;
  -h|--help)
    sed -n '2,30p' "$0"; exit 0 ;;
  "") ;;
  *) fail "unknown option: $1 (try --check or --help)" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq   >/dev/null 2>&1 || fail "jq is required"
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 \
  || fail "sha256sum or shasum is required"

sha256_verify() { # sha256_verify <file> <expected-hash>
  local got
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$1" | awk '{print $1}')"
  else
    got="$(shasum -a 256 "$1" | awk '{print $1}')"
  fi
  [[ "$got" == "$2" ]]
}

# ── Platform ──────────────────────────────────────────────────────────
case "$(uname -s)" in
  Linux)  GOOS="linux" ;;
  Darwin) GOOS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
  *) fail "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) GOARCH="amd64" ;;
  arm64|aarch64) GOARCH="arm64" ;;
  *) fail "unsupported arch: $(uname -m)" ;;
esac

# ── Versions ──────────────────────────────────────────────────────────
installed_version() {
  if [[ -f "${HOME_DIR}/version.json" ]]; then
    jq -r '.version // empty' "${HOME_DIR}/version.json" 2>/dev/null
  fi
}

CURRENT="$(installed_version || true)"
[[ -n "$CURRENT" ]] || CURRENT="unknown"

log "installed: ${CURRENT} (${GOOS}/${GOARCH})"

release_json="$(curl -fsSL --max-time 20 "$API" 2>/dev/null)" \
  || fail "no GitHub Release found for ${REPO} (or the API is unreachable). Publish a v* tag first — see .github/workflows/release.yml"

LATEST="$(jq -r '.tag_name // empty' <<<"$release_json")"
[[ -n "$LATEST" ]] || fail "release payload has no tag_name"
LATEST_V="${LATEST#v}"
log "latest:    ${LATEST_V}"

ASSET="omniproxy-${LATEST_V}-${GOOS}-${GOARCH}.tar.gz"
ASSET_URL="$(jq -r --arg n "$ASSET" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
SHA_URL="$(jq -r --arg n "${ASSET}.sha256" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
[[ -n "$ASSET_URL" ]] || fail "release ${LATEST} has no asset ${ASSET}"

if [[ "$CURRENT" == "$LATEST_V" && "$MODE" == "check" ]]; then
  log "up to date."
  exit 0
fi
if [[ "$MODE" == "check" ]]; then
  log "update available: ${CURRENT} -> ${LATEST_V}"
  exit 0
fi
if [[ "$CURRENT" == "$LATEST_V" ]]; then
  log "already at ${LATEST_V}; nothing to do (delete ${HOME_DIR}/version.json to force)."
  exit 0
fi
[[ -n "$SHA_URL" ]] || fail "release ${LATEST} has no ${ASSET}.sha256 — refusing to install an unverifiable binary"

# ── Download + verify ─────────────────────────────────────────────────
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "downloading ${ASSET}..."
curl -fsSL --max-time 300 -o "${TMP}/${ASSET}" "$ASSET_URL" || fail "download failed"
curl -fsSL --max-time 60  -o "${TMP}/${ASSET}.sha256" "$SHA_URL" || fail "checksum download failed"

EXPECTED="$(awk '{print $1}' "${TMP}/${ASSET}.sha256")"
[[ -n "$EXPECTED" ]] || fail "empty checksum file"
sha256_verify "${TMP}/${ASSET}" "$EXPECTED" \
  || fail "sha256 mismatch — downloaded file is corrupt or tampered with; aborting"
log "sha256 verified: ${EXPECTED}"

tar xzf "${TMP}/${ASSET}" -C "$TMP" || fail "tar extract failed"
SRC="${TMP}/omniproxy-${LATEST_V}-${GOOS}-${GOARCH}"
[[ -d "$SRC" ]] || fail "unexpected tarball layout (no ${SRC##*/}/ directory)"
NEW_BIN="${SRC}/omniproxy"
[[ "$GOOS" == "windows" ]] && NEW_BIN="${SRC}/omniproxy.exe"
[[ -f "$NEW_BIN" ]] || fail "tarball contains no binary"

# ── Backup + swap ─────────────────────────────────────────────────────
ROLLBACK_DIR="${HOME_DIR}/.rollback"
mkdir -p "${HOME_DIR}/bin" "$ROLLBACK_DIR"

BACKUP=""
if [[ -f "$BIN" ]]; then
  BACKUP="${ROLLBACK_DIR}/omniproxy.$(date +%Y%m%d_%H%M%S)"
  cp -p "$BIN" "$BACKUP"
  log "backed up current binary -> ${BACKUP}"
fi

install -m 0755 "$NEW_BIN" "${BIN}.new"
mv "${BIN}.new" "$BIN"

if [[ -d "${SRC}/web" ]]; then
  rm -rf "${WEB}.new"
  cp -r "${SRC}/web" "${WEB}.new"
  rm -rf "${WEB}.old"
  [[ -d "$WEB" ]] && mv "$WEB" "${WEB}.old"
  mv "${WEB}.new" "$WEB"
  rm -rf "${WEB}.old"
  log "web assets updated"
fi

cp "${SRC}/version.json" "${HOME_DIR}/version.json" 2>/dev/null \
  || printf '{"version": "%s"}\n' "$LATEST_V" > "${HOME_DIR}/version.json"
log "installed ${LATEST_V}"

# prune old rollbacks (never removes the one just made if it is the newest)
ls -1t "${ROLLBACK_DIR}"/omniproxy.* 2>/dev/null | tail -n +"$((KEEP_ROLLBACKS + 1))" | while read -r old; do
  rm -f "$old" && log "pruned old rollback ${old##*/}"
done

# ── Restart ───────────────────────────────────────────────────────────
restart() {
  if [[ -n "$RESTART_CMD" ]]; then
    log "restarting via OMNIPROXY_RESTART_CMD..."
    bash -c "$RESTART_CMD"
  elif command -v systemctl >/dev/null 2>&1 && systemctl --user cat "$SERVICE" >/dev/null 2>&1; then
    log "restarting ${SERVICE} (systemd --user)..."
    systemctl --user restart "$SERVICE"
  else
    fail "no restart mechanism found: set OMNIPROXY_RESTART_CMD or install the systemd user unit (${SERVICE}). The new binary is in place; start it manually."
  fi
}

health_ok() { # health_ok <seconds>
  local deadline=$(( $(date +%s) + ${1:-30} )) code
  while (( $(date +%s) < deadline )); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$HEALTH_URL" 2>/dev/null || true)"
    # Any HTTP answer below 500 proves the server is up and serving — including
    # 401/403 when the health route is key-protected. 000 = no listener,
    # 5xx = up but unhealthy (e.g. failing startup).
    if [[ -n "$code" && "$code" != "000" && "$code" -lt 500 ]]; then
      log "health-check OK (${HEALTH_URL} -> ${code})"; return 0
    fi
    sleep 2
  done
  return 1
}

restart
if health_ok 30; then
  log "update complete: ${CURRENT} -> ${LATEST_V}"
  exit 0
fi

# ── Rollback ──────────────────────────────────────────────────────────
log "health-check FAILED — rolling back..."
if [[ -n "$BACKUP" && -f "$BACKUP" ]]; then
  install -m 0755 "$BACKUP" "${BIN}.rollback"
  mv "${BIN}.rollback" "$BIN"
  restart || true
  if health_ok 20; then
    fail "update to ${LATEST_V} rolled back to ${CURRENT}; service is healthy again"
  fi
  fail "update to ${LATEST_V} rolled back to ${CURRENT} but the service is STILL unhealthy — check $(dirname "$BIN")/../logs and start it manually"
fi
fail "health-check failed and no backup exists to roll back to — check the logs under ${HOME_DIR}/logs"
