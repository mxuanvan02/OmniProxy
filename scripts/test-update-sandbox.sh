#!/usr/bin/env bash
# Sandbox test for scripts/update.sh — fake GitHub release API + fake binary,
# real HTTP server on ephemeral ports, no live service touched.
#
# Coverage:
#   T1  --check detects an available update
#   T2  unhealthy new version -> rollback: binary + web restored,
#       version.json KEEPS the old version, exit non-zero
#   T3  healthy path -> binary/web/version.json updated, exit 0
#   T4  already up to date -> no-op
#   T5  tampered tarball -> sha256 mismatch, install refused
#   T6  first run: no existing binary, empty .rollback -> prune must not
#       kill the script (pipefail trap), install succeeds
#   T7  restart command fails -> rollback path, exit non-zero
#   T8  prune keeps the just-made backup even when older backups have
#       NEWER mtimes (name-sort, not mtime-sort), KEEP_ROLLBACKS honored
#   T9  after a rollback, --check still reports the update (retry path alive)
#   T10 repo layout: binary/web swap at ${HOME}/omniproxy (no bin/), --layout repo
#
# The suite derives the release asset name from uname, matching how update.sh
# resolves it, so it runs on darwin/arm64 as well as linux/amd64. Hardcoding
# linux-amd64 made every case fail on macOS at the asset lookup.
#
# Note: exercises the sha256sum branch only; the macOS shasum branch shares
# the same `awk '{print $1}'` shape but is not covered here.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"
ASSET_PID=""
HEALTH_PID=""
cleanup() {
  [[ -n "$ASSET_PID" ]]  && kill "$ASSET_PID"  2>/dev/null || true
  [[ -n "$HEALTH_PID" ]] && kill "$HEALTH_PID" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

pick_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }
APORT="$(pick_port)"
HPORT="$(pick_port)"
HEALTH_FLAG="$T/health-ok"   # test-private, not world-writable /tmp

# Same platform resolution update.sh uses, so the fake release ships the asset
# this machine will actually ask for.
case "$(uname -s)" in
  Linux)  GOOS="linux" ;;
  Darwin) GOOS="darwin" ;;
  *) echo "unsupported OS for this test: $(uname -s)"; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) GOARCH="amd64" ;;
  arm64|aarch64) GOARCH="arm64" ;;
  *) echo "unsupported arch for this test: $(uname -m)"; exit 1 ;;
esac
ASSET="omniproxy-0.5.0-${GOOS}-${GOARCH}.tar.gz"
PKGNAME="omniproxy-0.5.0-${GOOS}-${GOARCH}"

echo "== setup fake release =="
mkdir -p "$T/assets" "$T/home/bin"
# fake "binary" 0.4.0 (current) and 0.5.0 (new)
printf '#!/bin/sh\necho old-0.4.0\n' > "$T/home/bin/omniproxy"
chmod +x "$T/home/bin/omniproxy"
printf '{"version":"0.4.0"}\n' > "$T/home/version.json"
mkdir -p "$T/home/bin/web"; echo old > "$T/home/bin/web/index.html"

# build fake tarball like the workflow does
PKG="$T/pkg/${PKGNAME}"
mkdir -p "$PKG/web"
printf '#!/bin/sh\necho new-0.5.0\n' > "$PKG/omniproxy"; chmod +x "$PKG/omniproxy"
echo new > "$PKG/web/index.html"
printf '{"version":"0.5.0"}\n' > "$PKG/version.json"
mkdir -p "$PKG/scripts"
printf '#!/bin/sh\necho fake-updater\n' > "$PKG/scripts/update.sh"; chmod +x "$PKG/scripts/update.sh"
tar czf "$T/assets/${ASSET}" -C "$T/pkg" "${PKGNAME}"
( cd "$T/assets" && sha256sum "${ASSET}" > "${ASSET}.sha256" )
# keep a pristine copy: T5 corrupts the served tarball
mkdir -p "$T/pristine" && cp "$T/assets"/* "$T/pristine/"

# release JSON (server root is $T, assets live under $T/assets/)
python3 - "$T/assets" "$APORT" > "$T/release.json" <<'EOF'
import json, os, sys
d, port = sys.argv[1], sys.argv[2]
assets = [{"name": f, "browser_download_url": f"http://127.0.0.1:{port}/assets/" + f} for f in sorted(os.listdir(d))]
json.dump({"tag_name": "v0.5.0", "assets": assets}, sys.stdout)
EOF

# serve assets + release json (exec so $! is python itself — killable)
( cd "$T" && exec python3 -m http.server "$APORT" --bind 127.0.0.1 ) >/dev/null 2>&1 &
ASSET_PID=$!
for _ in $(seq 1 20); do
  curl -fs -o /dev/null "http://127.0.0.1:${APORT}/release.json" 2>/dev/null && break
  sleep 0.5
done
curl -fs "http://127.0.0.1:${APORT}/release.json" >/dev/null || { echo "FAIL: asset server not up"; exit 1; }

# fake health server — pass/fail controlled by HEALTH_FLAG existence
python3 - "$HEALTH_FLAG" "$HPORT" <<'EOF' &
import http.server, os, sys
flag, port = sys.argv[1], int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if os.path.exists(flag) else 503); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
EOF
HEALTH_PID=$!
for _ in $(seq 1 20); do
  curl -s -o /dev/null "http://127.0.0.1:${HPORT}/" 2>/dev/null && break
  sleep 0.5
done

export OMNIPROXY_HOME="$T/home"
export OMNIPROXY_API_URL="http://127.0.0.1:${APORT}/release.json"
export OMNIPROXY_RESTART_CMD="true"
export OMNIPROXY_HEALTH_URL="http://127.0.0.1:${HPORT}/v1/models"
export OMNIPROXY_PORT="$HPORT"

UPDATE="$REPO_ROOT/scripts/update.sh"
reset_state() { # back to installed-0.4.0 healthy baseline
  printf '#!/bin/sh\necho old-0.4.0\n' > "$T/home/bin/omniproxy"; chmod +x "$T/home/bin/omniproxy"
  printf '{"version":"0.4.0"}\n' > "$T/home/version.json"
  mkdir -p "$T/home/bin/web"; echo old > "$T/home/bin/web/index.html"
  rm -rf "$T/home/.rollback" "$T/home/bin/web.prev-update"
  touch "$HEALTH_FLAG"
}

echo "== TEST 1: --check detects update =="
OUT=$("$UPDATE" --check 2>&1) || { echo "$OUT"; echo "FAIL t1"; exit 1; }
echo "$OUT" | grep -q "update available: 0.4.0 -> 0.5.0" || { echo "$OUT"; echo "FAIL t1 msg"; exit 1; }
echo PASS t1

echo "== TEST 2: unhealthy new version -> rollback (binary+web+version.json kept old) =="
reset_state
rm -f "$HEALTH_FLAG"
OUT=$("$UPDATE" 2>&1) && RC=0 || RC=$?
echo "$OUT"
[[ $RC -ne 0 ]] || { echo "FAIL t2: should exit non-zero on unhealthy"; exit 1; }
echo "$OUT" | grep -q "sha256 verified" || { echo "FAIL t2 sha"; exit 1; }
echo "$OUT" | grep -q "rolling back" || { echo "FAIL t2 rollback"; exit 1; }
"$T/home/bin/omniproxy" | grep -q old-0.4.0 || { echo "FAIL t2: binary not restored"; exit 1; }
grep -q old "$T/home/bin/web/index.html" || { echo "FAIL t2: web not rolled back"; exit 1; }
jq -r .version "$T/home/version.json" | grep -q '^0.4.0$' || { echo "FAIL t2: version.json must keep 0.4.0 after rollback"; exit 1; }
ls "$T/home/.rollback"/omniproxy.* >/dev/null || { echo "FAIL t2: backup missing"; exit 1; }
echo PASS t2

echo "== TEST 9: after rollback, --check still offers the update (retry alive) =="
OUT=$("$UPDATE" --check 2>&1) || { echo "$OUT"; echo "FAIL t9"; exit 1; }
echo "$OUT" | grep -q "update available: 0.4.0 -> 0.5.0" || { echo "$OUT"; echo "FAIL t9 msg"; exit 1; }
echo PASS t9

echo "== TEST 3: healthy path — swap succeeds =="
reset_state
OUT=$("$UPDATE" 2>&1) || { echo "$OUT"; echo "FAIL t3"; exit 1; }
echo "$OUT"
"$T/home/bin/omniproxy" | grep -q new-0.5.0 || { echo "FAIL t3: new binary not installed"; exit 1; }
jq -r .version "$T/home/version.json" | grep -q '^0.5.0$' || { echo "FAIL t3: version.json"; exit 1; }
grep -q new "$T/home/bin/web/index.html" || { echo "FAIL t3: web not updated"; exit 1; }
[[ ! -e "$T/home/bin/web.prev-update" ]] || { echo "FAIL t3: web.prev-update not cleaned"; exit 1; }
echo "$OUT" | grep -q "update complete: 0.4.0 -> 0.5.0" || { echo "FAIL t3 msg"; exit 1; }
echo PASS t3

echo "== TEST 4: already up to date =="
OUT=$("$UPDATE" 2>&1) || { echo "$OUT"; echo "FAIL t4"; exit 1; }
echo "$OUT" | grep -q "already at 0.5.0" || { echo "$OUT"; echo "FAIL t4 msg"; exit 1; }
echo PASS t4

echo "== TEST 6: first run — no existing binary, empty .rollback (prune must not kill script) =="
reset_state
rm -f "$T/home/bin/omniproxy" "$T/home/version.json"
OUT=$("$UPDATE" 2>&1) || { echo "$OUT"; echo "FAIL t6: fresh-install path died (exit $?)"; exit 1; }
echo "$OUT"
"$T/home/bin/omniproxy" | grep -q new-0.5.0 || { echo "FAIL t6: binary not installed"; exit 1; }
jq -r .version "$T/home/version.json" | grep -q '^0.5.0$' || { echo "FAIL t6: version.json not written"; exit 1; }
echo "$OUT" | grep -q "update complete: unknown -> 0.5.0" || { echo "$OUT"; echo "FAIL t6 msg"; exit 1; }
echo PASS t6

echo "== TEST 7: restart command fails -> rollback, non-zero exit =="
reset_state
OUT=$(OMNIPROXY_RESTART_CMD=false "$UPDATE" 2>&1) && RC=0 || RC=$?
echo "$OUT"
[[ $RC -ne 0 ]] || { echo "FAIL t7: restart failure must exit non-zero"; exit 1; }
echo "$OUT" | grep -q "rolling back" || { echo "FAIL t7: no rollback on restart failure"; exit 1; }
"$T/home/bin/omniproxy" | grep -q old-0.4.0 || { echo "FAIL t7: binary not restored"; exit 1; }
jq -r .version "$T/home/version.json" | grep -q '^0.4.0$' || { echo "FAIL t7: version.json must keep old"; exit 1; }
echo PASS t7

echo "== TEST 8: prune keeps the just-made backup (name-sort beats mtime) =="
reset_state
mkdir -p "$T/home/.rollback"
# 3 pre-existing backups whose mtimes are NEWER than the running binary's
# backup will be (cp without -p gives "now"; make these even newer via touch).
# -t, not -d: GNU date strings are rejected by BSD touch on macOS.
for d in 20250101 20250102 20250103; do
  printf 'stale\n' > "$T/home/.rollback/omniproxy.${d}_120000"
  touch -t 203001010000 "$T/home/.rollback/omniproxy.${d}_120000"   # mtime in the FUTURE
done
touch -t 202001010000 "$T/home/bin/omniproxy"   # running binary ancient by mtime
OUT=$(OMNIPROXY_KEEP_ROLLBACKS=3 "$UPDATE" 2>&1) || { echo "$OUT"; echo "FAIL t8 run"; exit 1; }
echo "$OUT"
NEWEST="$(ls -1 "$T/home/.rollback"/omniproxy.$(date +%Y)* 2>/dev/null || true)"
[[ -n "$NEWEST" ]] || { echo "FAIL t8: just-made backup was pruned (mtime-sort regression)"; exit 1; }
COUNT="$(ls -1 "$T/home/.rollback"/omniproxy.* | wc -l)"
[[ "$COUNT" -le 3 ]] || { echo "FAIL t8: KEEP_ROLLBACKS not honored (have $COUNT)"; exit 1; }
[[ ! -e "$T/home/.rollback/omniproxy.20250101_120000" ]] || { echo "FAIL t8: oldest-by-name not pruned"; exit 1; }
echo PASS t8

echo "== TEST 5: tampered tarball refused (sha mismatch) =="
reset_state
python3 - "$T/assets/${ASSET}" <<'EOF'
import sys
with open(sys.argv[1], "r+b") as f:
    f.seek(-10, 2); f.write(b"0123456789")  # corrupt tail
EOF
OUT=$("$UPDATE" 2>&1) && RC=0 || RC=$?
[[ $RC -ne 0 ]] || { echo "FAIL t5: corrupt tarball accepted"; exit 1; }
echo "$OUT" | grep -q "sha256 mismatch" || { echo "$OUT"; echo "FAIL t5 msg"; exit 1; }
# served asset corrupted; state must be untouched
"$T/home/bin/omniproxy" | grep -q old-0.4.0 || { echo "FAIL t5: binary touched despite refusal"; exit 1; }
cp "$T/pristine"/* "$T/assets/"   # restore for any later runs
echo PASS t5

# The layout this machine actually runs: binary + web/ directly in the repo
# dir, no bin/, restarted by launchd. Getting it wrong is the silent-failure
# mode this whole suite exists to catch — update.sh would install to
# $HOME/.omniproxy-user/bin/ while launchd keeps running the old binary.
R="$T/repo"
echo "== TEST 10: repo layout swaps ${R}/omniproxy, leaves repo scripts alone =="
rm -rf "$R"; mkdir -p "$R/web" "$R/scripts"
printf '#!/bin/sh\necho old-0.4.0\n' > "$R/omniproxy"; chmod +x "$R/omniproxy"
printf '{"version":"0.4.0"}\n' > "$R/version.json"
echo old > "$R/web/index.html"
printf '#!/bin/sh\necho git-tracked-updater\n' > "$R/scripts/update.sh"
OUT=$(OMNIPROXY_HOME="$R" OMNIPROXY_RESTART_CMD=true "$UPDATE" --layout repo 2>&1) || { echo "$OUT"; echo "FAIL t10: repo update failed"; exit 1; }
echo "$OUT"
"$R/omniproxy" | grep -q new-0.5.0 || { echo "FAIL t10: binary not swapped at ${R}/omniproxy"; exit 1; }
jq -r .version "$R/version.json" | grep -q '^0.5.0$' || { echo "FAIL t10: version.json not updated"; exit 1; }
grep -q new "$R/web/index.html" || { echo "FAIL t10: web not updated"; exit 1; }
# repo layout must NOT clobber the git-tracked updater with the tarball's copy
grep -q git-tracked-updater "$R/scripts/update.sh" || { echo "FAIL t10: repo scripts/ was overwritten"; exit 1; }
[[ ! -e "$R/bin" ]] || { echo "FAIL t10: stray ${R}/bin created in repo layout"; exit 1; }
echo PASS t10

echo "ALL TESTS PASSED (10/10)"
