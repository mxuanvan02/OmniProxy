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
#   5. Swaps in the new binary (atomic rename) and web/ (single-rename window;
#      the previous web/ is kept aside until the health check passes).
#   6. Restarts the service (systemd --user by default; override with
#      $OMNIPROXY_RESTART_CMD, e.g. launchctl on macOS).
#   7. Health-checks /v1/models; only on success writes version.json and prunes
#      old rollbacks. On ANY failure (restart error or unhealthy) it restores
#      binary + web, restarts again and exits non-zero. version.json keeps the
#      OLD version through a rollback, so a retry after fixing the problem
#      still sees the update as available.
#
# Env overrides:
#   OMNIPROXY_LAYOUT       prefix (default) | repo — where the binary and web/ live.
#                          prefix: $OMNIPROXY_HOME/bin/{omniproxy,web}  (installer layout)
#                          repo:   <clone>/{omniproxy,web}             (running from a git clone)
#                          `--layout repo` sets it too.
#   OMNIPROXY_REPO         default mxuanvan02/OmniProxy
#   OMNIPROXY_HOME         default ~/.omniproxy-user (prefix) / the clone root (repo)
#   OMNIPROXY_SERVICE      default omniproxy-user.service (systemd --user)
#   OMNIPROXY_LAUNCHD_LABEL  default com.van.omniproxy (repo layout restart)
#   OMNIPROXY_PORT         default 8080                (health-check port)
#   OMNIPROXY_HEALTH_URL   default http://127.0.0.1:$OMNIPROXY_PORT/v1/models
#   OMNIPROXY_RESTART_CMD  custom restart command (skips systemd entirely)
#   OMNIPROXY_INSTALLED_VERSION  the running binary's version, when the caller
#                          knows it (the dashboard endpoint does). Overrides
#                          version.json, which in repo layout only describes the
#                          checkout and can be ahead of the live process.
#   OMNIPROXY_KEEP_ROLLBACKS  default 5 (older .rollback binaries are pruned)
#   OMNIPROXY_API_URL      override the release-API URL (testing/mirrors)

set -euo pipefail

log()  { echo "[update] $*"; }
fail() { echo "[update] ERROR: $*" >&2; exit 1; }

# ── Options ───────────────────────────────────────────────────────────
MODE="update"
LAYOUT_ARG=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check) MODE="check"; shift ;;
    --layout) LAYOUT_ARG="${2:-}"; [[ -n "$LAYOUT_ARG" ]] || fail "--layout needs a value"; shift 2 ;;
    --layout=*) LAYOUT_ARG="${1#--layout=}"; shift ;;
    -h|--help)
      # print the leading comment block only (stop at the first non-comment line)
      awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
      exit 0 ;;
    *) fail "unknown option: $1 (try --check, --layout, or --help)" ;;
  esac
done

# ── Layout ────────────────────────────────────────────────────────────
# Where the binary, web/ and version.json actually live. Getting this wrong is
# silent: the update "succeeds", writes a binary nobody executes, and the
# service keeps running the old one. So it is explicit, never guessed.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LAYOUT="${LAYOUT_ARG:-${OMNIPROXY_LAYOUT:-prefix}}"
case "$LAYOUT" in
  prefix|repo) ;;
  *) fail "unknown layout: ${LAYOUT} (expected prefix or repo)" ;;
esac

REPO="${OMNIPROXY_REPO:-mxuanvan02/OmniProxy}"
if [[ "$LAYOUT" == "repo" ]]; then
  HOME_DIR="${OMNIPROXY_HOME:-$(dirname "$SCRIPT_DIR")}"
  BIN="${HOME_DIR}/omniproxy"
  WEB="${HOME_DIR}/web"
else
  HOME_DIR="${OMNIPROXY_HOME:-$HOME/.omniproxy-user}"
  BIN="${HOME_DIR}/bin/omniproxy"
  WEB="${HOME_DIR}/bin/web"
fi
SERVICE="${OMNIPROXY_SERVICE:-omniproxy-user.service}"
LAUNCHD_LABEL="${OMNIPROXY_LAUNCHD_LABEL:-com.van.omniproxy}"
PORT="${OMNIPROXY_PORT:-8080}"
HEALTH_URL="${OMNIPROXY_HEALTH_URL:-http://127.0.0.1:${PORT}/v1/models}"
RESTART_CMD="${OMNIPROXY_RESTART_CMD:-}"
KEEP_ROLLBACKS="${OMNIPROXY_KEEP_ROLLBACKS:-5}"
API="${OMNIPROXY_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"

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
if [[ "$GOOS" == "windows" ]]; then
  # Releases ship windows/amd64 only, and the systemd restart path does not
  # exist there — require an explicit restart command up front.
  [[ "$GOARCH" == "amd64" ]] || fail "windows/${GOARCH} is not built by the release workflow (amd64 only)"
  [[ -n "$RESTART_CMD" ]] || fail "on Windows set OMNIPROXY_RESTART_CMD (no systemd restart path)"
fi

# ── Versions ──────────────────────────────────────────────────────────
# OMNIPROXY_INSTALLED_VERSION is the version of the process that invoked us,
# set by the dashboard's update endpoint. It wins over version.json because in
# repo layout that file describes the CHECKOUT, not the running process: right
# after a release commit bumps it, version.json already reads the new tag while
# the service is still the older binary it started as. Trusting the file there
# makes this script report "already up to date", exit 0, and tell the dashboard
# the update succeeded without swapping anything.
installed_version() {
  if [[ -n "${OMNIPROXY_INSTALLED_VERSION:-}" ]]; then
    printf '%s\n' "$OMNIPROXY_INSTALLED_VERSION"
    return
  fi
  if [[ -f "${HOME_DIR}/version.json" ]]; then
    jq -r '.version // empty' "${HOME_DIR}/version.json" 2>/dev/null
  fi
}

CURRENT="$(installed_version || true)"
[[ -n "$CURRENT" ]] || CURRENT="unknown"

log "installed: ${CURRENT} (${GOOS}/${GOARCH})"

release_json="$(curl -fsSL --max-time 20 "$API" 2>/dev/null)" \
  || fail "no GitHub Release found for ${REPO} (or the API is unreachable/rate-limited). Publish a v* tag first — see .github/workflows/release.yml"

LATEST="$(jq -r '.tag_name // empty' <<<"$release_json")"
[[ -n "$LATEST" ]] || fail "release payload has no tag_name"
LATEST_V="${LATEST#v}"
log "latest:    ${LATEST_V}"

ASSET="omniproxy-${LATEST_V}-${GOOS}-${GOARCH}.tar.gz"
ASSET_URL="$(jq -r --arg n "$ASSET" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
SHA_URL="$(jq -r --arg n "${ASSET}.sha256" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
[[ -n "$ASSET_URL" ]] || fail "release ${LATEST} has no asset ${ASSET}"
# Gate in BOTH modes: an update that would be refused for a missing checksum
# must not be announced as available.
[[ -n "$SHA_URL" ]] || fail "release ${LATEST} has no ${ASSET}.sha256 — refusing to install an unverifiable binary"

if [[ "$MODE" == "check" ]]; then
  if [[ "$CURRENT" == "$LATEST_V" ]]; then
    log "up to date."
  else
    log "update available: ${CURRENT} -> ${LATEST_V}"
  fi
  exit 0
fi
if [[ "$CURRENT" == "$LATEST_V" ]]; then
  log "already at ${LATEST_V}; nothing to do (delete ${HOME_DIR}/version.json to force)."
  exit 0
fi

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
# Derive the binary's own directory instead of assuming ${HOME_DIR}/bin: in
# repo layout the binary sits directly in the repo dir, and an unconditional
# mkdir would leave an empty bin/ inside the git working tree.
mkdir -p "$(dirname "$BIN")" "$ROLLBACK_DIR"

BACKUP=""
if [[ -f "$BIN" ]]; then
  BACKUP="${ROLLBACK_DIR}/omniproxy.$(date +%Y%m%d_%H%M%S)"
  cp "$BIN" "$BACKUP"
  log "backed up current binary -> ${BACKUP}"
fi

WEB_PREV="${WEB}.prev-update"
rm -rf "$WEB_PREV"

# The updater has to install itself: the dashboard's one-click update runs
# scripts/update.sh beside the binary, so a tarball install that never copies it
# can see a new release but never install one. Repo layout is skipped — there
# scripts/ is the git-tracked tree that is already running.
SCRIPTS="$(dirname "$WEB")/scripts"
SCRIPTS_PREV="${SCRIPTS}.prev-update"
rm -rf "$SCRIPTS_PREV"

install -m 0755 "$NEW_BIN" "${BIN}.new"
mv "${BIN}.new" "$BIN"

if [[ -d "${SRC}/web" ]]; then
  rm -rf "${WEB}.new"
  cp -r "${SRC}/web" "${WEB}.new"
  if [[ -d "$WEB" ]]; then
    mv "$WEB" "$WEB_PREV"      # kept until the health check passes
  fi
  mv "${WEB}.new" "$WEB"       # single-rename window
  log "web assets updated (previous kept at ${WEB_PREV##*/} until health-check)"
fi

if [[ "$LAYOUT" == "prefix" && -d "${SRC}/scripts" ]]; then
  rm -rf "${SCRIPTS}.new"
  cp -r "${SRC}/scripts" "${SCRIPTS}.new"
  if [[ -d "$SCRIPTS" ]]; then
    mv "$SCRIPTS" "$SCRIPTS_PREV"
  fi
  mv "${SCRIPTS}.new" "$SCRIPTS"
  log "updater scripts installed at ${SCRIPTS}"
fi

# Commit the version claim now, before the restart: the dashboard's update
# wrapper is a child of the very process the restart kills, so under systemd
# the updater can never survive to commit afterwards. VERSION_PREV lets
# rollback() put the old claim back; a run killed mid-restart then reports
# the version the process now serving actually runs.
VERSION_FILE="${HOME_DIR}/version.json"
VERSION_PREV=""
if [[ -f "$VERSION_FILE" ]]; then
  VERSION_PREV="${VERSION_FILE}.prev-update"
  cp "$VERSION_FILE" "$VERSION_PREV"
fi
if [[ -f "${SRC}/version.json" ]]; then
  cp "${SRC}/version.json" "$VERSION_FILE"
else
  printf '{"version": "%s"}\n' "$LATEST_V" > "$VERSION_FILE"
fi

# prune old rollbacks — by NAME (the timestamp is in the filename), never by
# mtime: `cp` backups can carry an old mtime and an mtime sort would delete
# the just-made backup. Empty dir must not kill the script under pipefail.
while IFS= read -r old; do
  [[ -n "$old" && -f "$old" && "$old" != "$BACKUP" ]] || continue
  rm -f "$old" && log "pruned old rollback ${old##*/}"
done < <(ls -1 "${ROLLBACK_DIR}"/omniproxy.* 2>/dev/null | sort -r | tail -n +"$((KEEP_ROLLBACKS + 1))" || true)

# ── Restart ───────────────────────────────────────────────────────────
restart() {
  if [[ -n "$RESTART_CMD" ]]; then
    log "restarting via OMNIPROXY_RESTART_CMD..."
    bash -c "$RESTART_CMD"
  elif [[ "$LAYOUT" == "repo" ]] && command -v launchctl >/dev/null 2>&1; then
    # -k kills the running instance and starts it again, so the swapped binary
    # is what comes up. A plain `launchctl start` would be a no-op on a job that
    # is already running — the old process would keep serving the old binary.
    log "restarting via launchd (${LAUNCHD_LABEL})..."
    launchctl kickstart -k "gui/$(id -u)/${LAUNCHD_LABEL}"
  elif command -v systemctl >/dev/null 2>&1 && systemctl --user cat "$SERVICE" >/dev/null 2>&1; then
    log "restarting ${SERVICE} (systemd --user)..."
    systemctl --user restart "$SERVICE"
  else
    fail "no restart mechanism found: set OMNIPROXY_RESTART_CMD, install the systemd user unit (${SERVICE}), or use --layout repo with the launchd job ${LAUNCHD_LABEL} loaded. The new binary is in place; start it manually."
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

rollback() { # restore binary + web, restart, report
  log "rolling back..."
  if [[ -n "$BACKUP" && -f "$BACKUP" ]]; then
    install -m 0755 "$BACKUP" "${BIN}.rollback"
    mv "${BIN}.rollback" "$BIN"
  else
    log "no previous binary to restore (fresh install); removing the failed binary"
    rm -f "$BIN"
  fi
  if [[ -d "$WEB_PREV" ]]; then
    rm -rf "$WEB"
    mv "$WEB_PREV" "$WEB"
    log "web assets rolled back"
  fi
  if [[ -n "$VERSION_PREV" && -f "$VERSION_PREV" ]]; then
    mv "$VERSION_PREV" "$VERSION_FILE"
    log "version.json rolled back"
  fi
  restart || log "restart after rollback failed — start the service manually"
  if health_ok 20; then
    fail "update to ${LATEST_V} rolled back to ${CURRENT}; service is healthy again (version.json restored, so re-running the update will retry)"
  fi
  fail "update to ${LATEST_V} rolled back but the service is STILL unhealthy — check ${HOME_DIR}/logs and start it manually"
}

if ! restart; then
  log "restart command failed"
  rollback
fi

if health_ok 30; then
  rm -rf "$WEB_PREV" "$VERSION_PREV" "${WEB}.old" "${WEB}.new"
  log "update complete: ${CURRENT} -> ${LATEST_V}"
  exit 0
fi

log "health-check FAILED"
rollback
