#!/bin/sh
set -e

# Guard the legacy /admin UI first: its classic scripts share one top-level
# scope, so a cross-file `const` collision kills a whole file at runtime while
# `node --check` still passes. See scripts/check-admin-scripts.js for details.
if command -v node >/dev/null 2>&1; then
  node scripts/check-admin-scripts.js
else
  echo "build.sh: node not found, skipping admin script check" >&2
fi

go build -o omniproxy .

 