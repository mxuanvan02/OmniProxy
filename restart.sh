#!/usr/bin/env bash
#
# restart.sh — khoi dong lai OmniProxy dung cach (qua launchd).
#
#   ./restart.sh            restart + tu kiem chung
#   ./restart.sh --status    chi xem trang thai, KHONG restart
#
# TUYET DOI KHONG chay `./omniproxy` bang tay khi da co ban dang chay:
# main.go goi CheckAndKillExisting() roi os.Exit(0) — no GIET ban dang chay
# rot thoat, KHONG bind cong. Ket qua: proxy tat han, moi request dang bay
# chet voi ECONNREFUSED. Script nay dung launchctl nen khong bi bay do.

set -uo pipefail

LABEL="${LABEL:-com.van.omniproxy}"
PORT=8080
HEALTH="http://127.0.0.1:${PORT}/v1/models"
REAL_HOME="${REAL_HOME:-/Users/van}"
PLIST="${REAL_HOME}/Library/LaunchAgents/${LABEL}.plist"
DOMAIN="gui/$(id -u)"
BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

lc() { HOME="$REAL_HOME" launchctl "$@"; }

job_loaded() { lc print "${DOMAIN}/${LABEL}" >/dev/null 2>&1; }

job_field() {
  lc print "${DOMAIN}/${LABEL}" 2>/dev/null \
    | awk -v k="$1" '$1==k && $2=="=" {print $3; exit}'
}

listener_pid() {
  lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null | head -1
}

health_code() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$HEALTH" 2>/dev/null
}

show_state() {
  local lpid ppid code
  lpid="$(listener_pid)"
  if [[ -n "$lpid" ]]; then
    ppid="$(ps -o ppid= -p "$lpid" 2>/dev/null | tr -d ' ')"
    echo "  cong ${PORT}      : PID ${lpid} (cha=${ppid:-?}$([[ "${ppid:-}" == "1" ]] && echo ', launchd quan' || echo ', CHAY TAY - chua duoc quan'))"
  else
    echo "  cong ${PORT}      : khong ai lang nghe"
  fi
  if job_loaded; then
    echo "  launchd job    : $(job_field state) | pid=$(job_field pid) | so lan chay=$(job_field runs)"
  else
    echo "  launchd job    : CHUA NAP"
  fi
  code="$(health_code)"
  echo "  health         : HTTP ${code}$([[ "$code" == "200" ]] || echo '  <-- chua san sang')"
  echo "  binary         : $(stat -f '%Sm' -t '%F %T' "${BIN_DIR}/omniproxy" 2>/dev/null || echo 'khong thay')"
}

echo "=== OmniProxy: truoc khi restart ($(date '+%F %T')) ==="
show_state

if [[ "${1:-}" == "--status" || "${1:-}" == "-s" ]]; then
  exit 0
fi

# Neu dang co ban chay tay (cha khac launchd), dung graceful truoc.
# Menu mode co bat SIGTERM: flush config + drain request toi 10s.
hand_pid=""
for p in $(pgrep -x omniproxy 2>/dev/null); do
  [[ "$(ps -o ppid= -p "$p" | tr -d ' ')" != "1" ]] && hand_pid="$p"
done
if [[ -n "$hand_pid" ]]; then
  echo
  echo "-> phat hien ban chay tay (PID ${hand_pid}); dung graceful bang SIGTERM"
  kill -TERM "$hand_pid" 2>/dev/null
  for _ in $(seq 1 15); do
    kill -0 "$hand_pid" 2>/dev/null || break
    sleep 1
  done
  kill -0 "$hand_pid" 2>/dev/null && echo "   (van con song, launchd se xu ly tiep)"
fi

echo
if job_loaded; then
  echo "-> launchctl kickstart -k ${DOMAIN}/${LABEL}"
  # Loi kickstart KHONG duoc nuot: neu no that bai (vd sandbox chan, job sai
  # domain), tien trinh cu van listen + health 200, nen phan kiem chung ben
  # duoi se bao "OK" trong khi binary CU van dang chay. Da tung xay ra.
  if ! lc kickstart -k "${DOMAIN}/${LABEL}"; then
    echo "!! launchctl kickstart that bai — tien trinh cu (neu con) van dang chay." >&2
    echo "   Binary moi CHUA duoc nap. Thu lai ngoai sandbox." >&2
    exit 1
  fi
else
  if [[ ! -f "$PLIST" ]]; then
    echo "!! khong thay plist: ${PLIST}" >&2
    exit 1
  fi
  echo "-> launchctl bootstrap ${DOMAIN} ${PLIST}"
  lc bootstrap "$DOMAIN" "$PLIST" >/dev/null 2>&1
fi

# Doi bind cong + health 200
ok=""
for _ in $(seq 1 25); do
  sleep 1
  [[ -n "$(listener_pid)" ]] || continue
  [[ "$(health_code)" == "200" ]] && { ok="yes"; break; }
done

# Kiem tra da hoi tu chua: so lan chay khong duoc tang them
runs_a="$(job_field runs)"
sleep 5
runs_b="$(job_field runs)"

echo
echo "=== sau khi restart ==="
show_state
if [[ "$runs_a" != "$runs_b" ]]; then
  echo "  !! so lan chay tang ${runs_a} -> ${runs_b}: dang lap kill/respawn, xem data/omniproxy.launchd.out.log"
  exit 1
fi

if [[ -n "$ok" ]]; then
  echo
  echo "OK — proxy da len, launchd dang quan, health 200."
else
  echo
  echo "!! chua health 200 sau 25s. Xem log: data/omniproxy.launchd.err.log" >&2
  exit 1
fi
