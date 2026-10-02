#!/usr/bin/env bash
# Sandbox test for scripts/update.sh — fake GitHub release API + fake binary,
# real HTTP server, no live service touched.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$T"' EXIT

echo "== setup fake release =="
mkdir -p "$T/assets" "$T/home/bin"
# fake "binary" 0.4.0 (current) and 0.5.0 (new)
printf '#!/bin/sh\necho old-0.4.0\n' > "$T/home/bin/omniproxy"
chmod +x "$T/home/bin/omniproxy"
printf '{"version":"0.4.0"}\n' > "$T/home/version.json"
mkdir -p "$T/home/bin/web"; echo old > "$T/home/bin/web/index.html"

# build fake tarball like the workflow does
PKG="$T/pkg/omniproxy-0.5.0-linux-amd64"
mkdir -p "$PKG/web"
printf '#!/bin/sh\necho new-0.5.0\n' > "$PKG/omniproxy"; chmod +x "$PKG/omniproxy"
echo new > "$PKG/web/index.html"
printf '{"version":"0.5.0"}\n' > "$PKG/version.json"
tar czf "$T/assets/omniproxy-0.5.0-linux-amd64.tar.gz" -C "$T/pkg" omniproxy-0.5.0-linux-amd64
( cd "$T/assets" && sha256sum omniproxy-0.5.0-linux-amd64.tar.gz > omniproxy-0.5.0-linux-amd64.tar.gz.sha256 )

# release JSON (server root is $T, assets live under $T/assets/)
python3 - "$T/assets" > "$T/release.json" <<'EOF'
import json, os, sys
d = sys.argv[1]
assets = [{"name": f, "browser_download_url": "http://127.0.0.1:18099/assets/" + f} for f in sorted(os.listdir(d))]
json.dump({"tag_name": "v0.5.0", "assets": assets}, sys.stdout)
EOF

# serve assets + release json over one HTTP server
cp "$T/release.json" "$T/assets/../release.json" 2>/dev/null || true
( cd "$T" && python3 -m http.server 18099 --bind 127.0.0.1 >/dev/null 2>&1 ) &
for i in $(seq 1 20); do
  curl -fs -o /dev/null http://127.0.0.1:18099/release.json 2>/dev/null && break
  sleep 0.5
done
curl -fs http://127.0.0.1:18099/release.json >/dev/null || { echo "FAIL: test server not up"; exit 1; }

# fake health server (port 18098) — controllable pass/fail
python3 - <<'EOF' &
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = os.path.exists("/tmp/omni-test-health-ok")
        self.send_response(200 if ok else 503); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 18098), H).serve_forever()
EOF
for i in $(seq 1 20); do
  curl -s -o /dev/null http://127.0.0.1:18098/ 2>/dev/null && break
  sleep 0.5
done

export OMNIPROXY_HOME="$T/home"
export OMNIPROXY_API_URL="http://127.0.0.1:18099/release.json"
export OMNIPROXY_RESTART_CMD="true"
export OMNIPROXY_HEALTH_URL="http://127.0.0.1:18098/v1/models"
export OMNIPROXY_PORT=18098

echo "== TEST 1: --check detects update =="
OUT=$("$REPO_ROOT/scripts/update.sh" --check 2>&1) || { echo "$OUT"; echo "FAIL t1"; exit 1; }
echo "$OUT" | grep -q "update available: 0.4.0 -> 0.5.0" || { echo "$OUT"; echo "FAIL t1 msg"; exit 1; }
echo PASS t1

echo "== TEST 2: --check up-to-date after install / rollback path (health FAIL) =="
rm -f /tmp/omni-test-health-ok
OUT=$("$REPO_ROOT/scripts/update.sh" 2>&1) && RC=0 || RC=$?
echo "$OUT"
[[ $RC -ne 0 ]] || { echo "FAIL t2: should exit non-zero on unhealthy"; exit 1; }
echo "$OUT" | grep -q "sha256 verified" || { echo "FAIL t2 sha"; exit 1; }
echo "$OUT" | grep -q "rolling back" || { echo "FAIL t2 rollback"; exit 1; }
# binary restored to old version?
"$T/home/bin/omniproxy" | grep -q old-0.4.0 || { echo "FAIL t2: binary not restored"; exit 1; }
# version.json says 0.5.0 (swap happened) but binary rolled back — acceptable; check backup kept
ls "$T/home/.rollback"/omniproxy.* >/dev/null || { echo "FAIL t2: backup missing"; exit 1; }
echo PASS t2

echo "== TEST 3: healthy path — swap succeeds =="
# reset state
printf '#!/bin/sh\necho old-0.4.0\n' > "$T/home/bin/omniproxy"; chmod +x "$T/home/bin/omniproxy"
printf '{"version":"0.4.0"}\n' > "$T/home/version.json"
touch /tmp/omni-test-health-ok
OUT=$("$REPO_ROOT/scripts/update.sh" 2>&1) || { echo "$OUT"; echo "FAIL t3"; exit 1; }
echo "$OUT"
"$T/home/bin/omniproxy" | grep -q new-0.5.0 || { echo "FAIL t3: new binary not installed"; exit 1; }
jq -r .version "$T/home/version.json" | grep -q 0.5.0 || { echo "FAIL t3: version.json"; exit 1; }
grep -q new "$T/home/bin/web/index.html" || { echo "FAIL t3: web not updated"; exit 1; }
echo "$OUT" | grep -q "update complete: 0.4.0 -> 0.5.0" || { echo "FAIL t3 msg"; exit 1; }
echo PASS t3

echo "== TEST 4: already up to date =="
OUT=$("$REPO_ROOT/scripts/update.sh" 2>&1) || { echo "$OUT"; echo "FAIL t4"; exit 1; }
echo "$OUT" | grep -q "already at 0.5.0" || { echo "$OUT"; echo "FAIL t4 msg"; exit 1; }
echo PASS t4

echo "== TEST 5: tampered tarball refused (sha mismatch) =="
printf '{"version":"0.4.0"}\n' > "$T/home/version.json"
python3 - "$T/assets/omniproxy-0.5.0-linux-amd64.tar.gz" <<'EOF'
import sys
with open(sys.argv[1], "r+b") as f:
    f.seek(-10, 2); f.write(b"0123456789")  # corrupt tail
EOF
OUT=$("$REPO_ROOT/scripts/update.sh" 2>&1) && RC=0 || RC=$?
[[ $RC -ne 0 ]] || { echo "FAIL t5: corrupt tarball accepted"; exit 1; }
echo "$OUT" | grep -q "sha256 mismatch" || { echo "$OUT"; echo "FAIL t5 msg"; exit 1; }
echo PASS t5

rm -f /tmp/omni-test-health-ok
echo "ALL TESTS PASSED"
