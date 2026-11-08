#!/usr/bin/env bash
# bootstrap-mac-update.sh — one-shot manual update for OmniProxy installs that
# predate the self-update endpoint (anything older than 0.6.x, e.g. 0.4.0).
#
# Old installs serve a UI whose "download" button is just a GitHub link, so the
# one-click updater cannot be reached from them. Run this once on the machine:
#
#   curl -fsSL https://raw.githubusercontent.com/mxuanvan02/OmniProxy/main/scripts/bootstrap-mac-update.sh | bash
#
# What it does:
#   1. Finds the running omniproxy process and its binary path.
#   2. Derives the install layout (prefix: .../bin/omniproxy, repo: binary in a
#      clone root with version.json beside it).
#   3. Detects the listen port and the restart mechanism (launchd label or
#      systemd --user unit).
#   4. Downloads the latest release tarball for this OS/arch, and hands off to
#      scripts/update.sh INSIDE that tarball (sha256 verify, backup, atomic
#      swap, restart, health-check, rollback on failure).
#
# After this runs once, the dashboard's own Update button handles every future
# release — no GitHub round-trips.
#
# Env overrides (mostly for testing):
#   OMNIPROXY_REPO            default mxuanvan02/OmniProxy
#   OMNIPROXY_BOOTSTRAP_EXE   skip process detection, use this binary path
#   OMNIPROXY_BOOTSTRAP_HOME  skip layout derivation, use this home dir
#   OMNIPROXY_PORT            skip port detection, health-check this port
#   OMNIPROXY_RESTART_CMD     custom restart command (skips launchd/systemd)
set -euo pipefail

log()  { echo "[bootstrap] $*"; }
fail() { echo "[bootstrap] ERROR: $*" >&2; exit 1; }

REPO="${OMNIPROXY_REPO:-mxuanvan02/OmniProxy}"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq   >/dev/null 2>&1 || fail "jq is required"

# ── 1. Locate the running binary ──────────────────────────────────────
EXE="${OMNIPROXY_BOOTSTRAP_EXE:-}"
if [[ -z "$EXE" ]]; then
  PID="$(pgrep -f 'omniproxy( |$)' | head -1 || true)"
  [[ -n "$PID" ]] || fail "no running omniproxy process found (pgrep came up empty); set OMNIPROXY_BOOTSTRAP_EXE to the binary path"
  
  # Try multiple methods to get absolute path (macOS often returns relative paths)
  EXE=""
  
  # Method 1: lsof - find the binary mapped as 'txt' (text segment)
  if command -v lsof >/dev/null 2>&1; then
    EXE="$(lsof -p "$PID" 2>/dev/null | awk '$4 == "txt" && $5 == "REG" {print $9; exit}')"
  fi
  
  # Method 2: ps -o command= (full command line, may have absolute path)
  if [[ -z "$EXE" || ! -f "$EXE" ]]; then
    EXE="$(ps -o command= -p "$PID" 2>/dev/null | awk '{print $1}')"
  fi
  
  # Method 3: ps -o comm= + resolve relative via cwd
  if [[ -z "$EXE" || ! -f "$EXE" ]]; then
    REL_EXE="$(ps -o comm= -p "$PID" 2>/dev/null | awk '{print $1}')"
    if [[ -n "$REL_EXE" && "$REL_EXE" != /* ]]; then
      # Relative path — resolve via process cwd
      CWD="$(lsof -p "$PID" -Fn 2>/dev/null | awk '/^n\// && /cwd$/ {getline; sub(/^n/, ""); print; exit}')"
      if [[ -n "$CWD" && -f "$CWD/$REL_EXE" ]]; then
        EXE="$CWD/$REL_EXE"
      fi
    fi
  fi
  
  [[ -n "$EXE" && -f "$EXE" ]] || fail "could not resolve the binary path of PID $PID; set OMNIPROXY_BOOTSTRAP_EXE manually (run: ps aux | grep omniproxy)"
  log "running process: PID $PID -> $EXE"
fi
# Resolve symlinks so layout detection sees the real file location.
while [[ -L "$EXE" ]]; do EXE="$(readlink "$EXE")"; done
EXE_DIR="$(cd "$(dirname "$EXE")" && pwd)"

# ── 2. Layout + home dir ──────────────────────────────────────────────
HOME_DIR="${OMNIPROXY_BOOTSTRAP_HOME:-}"
LAYOUT=""
if [[ -n "$HOME_DIR" ]]; then
  # Explicit home: decide layout by where the binary sits inside it.
  if [[ "$EXE_DIR" == "$HOME_DIR/bin" ]]; then LAYOUT="prefix"; else LAYOUT="repo"; fi
elif [[ "$(basename "$EXE_DIR")" == "bin" ]]; then
  LAYOUT="prefix"
  HOME_DIR="$(dirname "$EXE_DIR")"
else
  LAYOUT="repo"
  HOME_DIR="$EXE_DIR"
fi
log "layout: $LAYOUT (home: $HOME_DIR)"

CURRENT="unknown"
if [[ -f "$HOME_DIR/version.json" ]]; then
  CURRENT="$(jq -r '.version // "unknown"' "$HOME_DIR/version.json")"
fi
log "installed version: $CURRENT"

# ── 3. Listen port (health-check target) ──────────────────────────────
PORT="${OMNIPROXY_PORT:-}"
if [[ -z "$PORT" ]]; then
  # Try the running process first; fall back to config.json; fall back to 8080.
  PID_NOW="$(pgrep -f 'omniproxy( |$)' | head -1 || true)"
  if [[ -n "$PID_NOW" ]] && command -v lsof >/dev/null 2>&1; then
    PORT="$(lsof -nP -iTCP -sTCP:LISTEN -a -p "$PID_NOW" 2>/dev/null \
      | awk 'NR>1 {split($9,a,":"); print a[length(a)]; exit}')"
  fi
  if [[ -z "$PORT" && -f "$HOME_DIR/data/config.json" ]]; then
    PORT="$(jq -r '.port // empty' "$HOME_DIR/data/config.json" 2>/dev/null || true)"
  fi
  [[ -n "$PORT" ]] || PORT="8080"
fi
log "port: $PORT"

# ── 4. Restart mechanism ──────────────────────────────────────────────
LAUNCHD_LABEL="${OMNIPROXY_LAUNCHD_LABEL:-}"
if [[ -z "${OMNIPROXY_RESTART_CMD:-}" && "$(uname -s)" == "Darwin" && -z "$LAUNCHD_LABEL" ]]; then
  LAUNCHD_LABEL="$(launchctl list 2>/dev/null | awk 'tolower($3) ~ /omniproxy|com\.van\./ {print $3; exit}')"
  if [[ -n "$LAUNCHD_LABEL" ]]; then
    log "launchd job: $LAUNCHD_LABEL"
    export OMNIPROXY_LAUNCHD_LABEL="$LAUNCHD_LABEL"
  fi
fi

# ── 5. Latest release, download, hand off to the tarball's update.sh ──
API="https://api.github.com/repos/${REPO}/releases/latest"
release_json="$(curl -fsSL --max-time 20 "$API")" || fail "GitHub release API unreachable ($API)"
LATEST="$(jq -r '.tag_name // empty' <<<"$release_json")"
[[ -n "$LATEST" ]] || fail "release payload has no tag_name"
LATEST_V="${LATEST#v}"
log "latest release: $LATEST_V"

if [[ "$CURRENT" == "$LATEST_V" ]]; then
  log "already at $LATEST_V; nothing to do."
  exit 0
fi

case "$(uname -s)" in
  Linux)  GOOS="linux" ;;
  Darwin) GOOS="darwin" ;;
  *) fail "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) GOARCH="amd64" ;;
  arm64|aarch64) GOARCH="arm64" ;;
  *) fail "unsupported arch: $(uname -m)" ;;
esac

ASSET="omniproxy-${LATEST_V}-${GOOS}-${GOARCH}.tar.gz"
ASSET_URL="$(jq -r --arg n "$ASSET" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
SHA_URL="$(jq -r --arg n "${ASSET}.sha256" '.assets[]? | select(.name==$n) | .browser_download_url' <<<"$release_json" | head -1)"
[[ -n "$ASSET_URL" ]] || fail "release $LATEST has no asset $ASSET"
[[ -n "$SHA_URL" ]]   || fail "release $LATEST has no ${ASSET}.sha256"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
log "downloading $ASSET ..."
curl -fsSL --max-time 300 -o "${TMP}/${ASSET}" "$ASSET_URL" || fail "download failed"
curl -fsSL --max-time 60  -o "${TMP}/${ASSET}.sha256" "$SHA_URL" || fail "checksum download failed"

EXPECTED="$(awk '{print $1}' "${TMP}/${ASSET}.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "${TMP}/${ASSET}" | awk '{print $1}')"
else
  GOT="$(shasum -a 256 "${TMP}/${ASSET}" | awk '{print $1}')"
fi
[[ "$GOT" == "$EXPECTED" ]] || fail "sha256 mismatch (got $GOT, want $EXPECTED) — refusing to install"
log "sha256 verified: $EXPECTED"

tar xzf "${TMP}/${ASSET}" -C "$TMP" || fail "tar extract failed"
SRC="${TMP}/omniproxy-${LATEST_V}-${GOOS}-${GOARCH}"
UPDATER="${SRC}/scripts/update.sh"
[[ -f "$UPDATER" ]] || fail "tarball has no scripts/update.sh"
chmod +x "$UPDATER"

log "running the release's own updater (verify -> backup -> swap -> restart -> health-check) ..."
OMNIPROXY_LAYOUT="$LAYOUT" \
OMNIPROXY_HOME="$HOME_DIR" \
OMNIPROXY_PORT="$PORT" \
OMNIPROXY_INSTALLED_VERSION="$CURRENT" \
bash "$UPDATER"
rc=$?
if [[ $rc -eq 0 ]]; then
  log "bootstrap complete: $CURRENT -> $LATEST_V"
  log "future updates: dashboard Settings -> Update button (fully automatic now)."
else
  fail "update.sh exited with code $rc (see its output above; a rollback restores the old install automatically)"
fi
