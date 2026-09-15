# Phase 07 — Build, embed and verify live

**Priority:** Critical — until this runs, none of phases 01–06 exists in the
running proxy.
**Status:** Not started
**Depends on:** phases 01–06
**Blocks:** nothing

## Context links

- `web-next/vite.config.ts:8-19` — `base: '/admin-next/'`, `outDir: '../webnext/dist'`,
  `emptyOutDir: true`, with the comment explaining why the output sits beside the
  Go file that embeds it
- `webnext/embed.go:19-20` — `//go:embed all:dist`; `:39` — `const Prefix = "/admin-next/"`
- `web-next/package.json:10-13` — `build` is `tsc -b && vite build`
- `build.sh` — one line: `go build -o omniproxy .`
- `restart.sh` (present on disk, gitignored) — launchd-only; its header warns in
  Vietnamese that running `./omniproxy` by hand while an instance is live makes
  `main.go`'s `CheckAndKillExisting()` kill the running proxy and exit without
  binding the port, dropping every in-flight request
- `webnext/dist/`'s six files are **tracked in git**
  (`git ls-files webnext/`), so the build output is part of this change
- `git ls-files webnext/dist` before the build: `assets/index-Bo4PJR6g.js`,
  `assets/UsageView-f3RAeHoC.js`, `assets/index-Ws5BLgvq.css`, `favicon.svg`,
  `icons.svg`, `index.html`

## Overview

Build the client, rebuild the Go binary so the embed picks up the new tree,
restart the launchd job, and prove that the bytes the running process serves are
the bytes that were just built.

## Key insights

**1. `./restart.sh` does not build.** It only kickstarts the launchd job
(`grep -n "build.sh\|npm\|go build" restart.sh` returns nothing). The order is
`npm run build` → `./build.sh` → `./restart.sh`. Running the restart alone
re-serves the previously embedded bundle and looks like a silent failure.

**2. The embed is compile-time.** `//go:embed all:dist` bakes the tree into the
binary at build time, so editing `webnext/dist/` under a running process changes
nothing until `./build.sh` runs and the job restarts. The proof therefore has to
be about the *served* bytes, not about the files on disk.

**3. Content-hashed asset names make the check unambiguous.** Vite names each
chunk `UsageView-<hash>.js`, so a rebuild changes the file name. A request for the
old name after a successful rebuild must 404, which is a much stronger signal
than comparing timestamps — and the name is what `index.html` and the entry chunk
reference, so its change is visible in the served HTML too.

**4. `go build` in this environment needs an explicit `GOCACHE`.**
`go env GOCACHE` is `/Users/van/Library/Caches/go-build`, outside the sandbox's
writable set, so the documented form is
`GOCACHE="$TMPDIR/gocache" ./build.sh`. If the default cache is writable on your
machine, the prefix is harmless.

**5. The build and the restart need to run outside the agent sandbox.** They
write `./omniproxy` and talk to launchd (`launchctl`, `lsof`), which the sandbox
refuses. Run them with `dangerouslyDisableSandbox: true` or from a normal
terminal; a sandbox denial here is expected, not a bug in the change.

**6. Never `./omniproxy` by hand.** It exits without binding the port when another
instance is live, having already killed it. This is `restart.sh`'s own warning and
it is the way a proxy goes down during a deploy.

**7. The build output is a tracked artefact.** `webnext/dist/**` is committed, so
the final commit includes six files whose hashed names changed. Do not
`.gitignore` them — `embed.go:41-43` documents that a checkout that never ran the
frontend build still compiles because `dist/` is committed.

## Requirements

**Functional**
- `web-next/` builds clean: `tsc -b` silent, `vite build` successful.
- `./build.sh` produces a new `omniproxy`.
- The launchd job is restarted through `./restart.sh`, and the proxy serves
  `/admin-next/`.
- The served bundle is byte-identical to the built one, contains the new page's
  markers, and no longer serves the previous `UsageView` chunk.
- The phase-01 fix is observable through the live API.

**Non-functional**
- No dependency installed or upgraded (`package.json` and `package-lock.json`
  unchanged from `HEAD`).
- No source file edited in this phase; a source change here means an earlier phase
  was incomplete, so fix it there and redo the build.
- The final commit contains the new asset names.

## Architecture

```
web-next/ ──npm run build──► webnext/dist/ ──./build.sh──► ./omniproxy ──./restart.sh──► launchd
                                     │                                              │
                                     └──── served from the embedded FS ◄───────────┘
                                            http://127.0.0.1:8080/admin-next/
```

## Related code files

- **Modify:** none — this phase only produces and ships artefacts
- **Rebuild (tracked, will change):** `webnext/dist/index.html`,
  `webnext/dist/assets/index-<newhash>.js`, `webnext/dist/assets/UsageView-<newhash>.js`,
  `webnext/dist/assets/index-<newhash>.css`
- **Rebuild (untracked):** `./omniproxy` (gitignored)

## Implementation steps

### Step 1: Capture the before state

```bash
cd /Users/van/Tools/OmniProxy
shasum -a 256 omniproxy
ls -1 webnext/dist/assets/
lsof -nP -iTCP:8080 -sTCP:LISTEN -t
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/admin-next/
```

Record the binary hash, the two asset names, the listening pid, and that the
client already serves. Without the "before" hash, "the binary was rebuilt" is
unprovable.

### Step 2: Build the client

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -12
npx tsc -b && npm run lint 2>&1 | tail -10
npm run build 2>&1 | tail -20
```

Expected: every suite green (the baseline was 8 files / 50 tests; it now includes
`usage.test.ts` and the six component suites), `tsc -b` silent, oxlint silent, and
Vite reporting a new `assets/UsageView-<hash>.js`. `emptyOutDir: true` means the
old hashed files are gone — confirm with `ls -1 webnext/dist/assets/`.

### Step 3: Rebuild the binary

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" ./build.sh
shasum -a 256 omniproxy
strings omniproxy | grep -c 'Token hiệu dụng'
```

Expected: the hash differs from step 1, and the marker count is at least 1 — the
new page's own Vietnamese label is now inside the binary. A count of 0 means the
embed did not pick up `webnext/dist/`, so check step 2 wrote there and that
`webnext/embed.go:19` still says `all:dist`.

Run this and step 4 with the sandbox disabled (Key insight 5).

### Step 4: Restart through launchd

```bash
cd /Users/van/Tools/OmniProxy
./restart.sh
lsof -nP -iTCP:8080 -sTCP:LISTEN -t
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/models
```

Expected: `restart.sh` completes its own self-check, the listening pid differs
from step 1, and the health endpoint answers. If the pid is unchanged, the job was
not restarted and the old embed is still being served.

### Step 5: Prove the served bundle is the built bundle

```bash
cd /Users/van/Tools/OmniProxy
NEW=$(basename "$(ls -1 webnext/dist/assets/UsageView-*.js)")
shasum -a 256 "webnext/dist/assets/$NEW"
curl -s "http://127.0.0.1:8080/admin-next/assets/$NEW" | shasum -a 256
curl -s -o /dev/null -w '%{http_code}\n' "http://127.0.0.1:8080/admin-next/assets/UsageView-f3RAeHoC.js"
curl -s "http://127.0.0.1:8080/admin-next/assets/$NEW" | grep -c 'Token hiệu dụng'
curl -s http://127.0.0.1:8080/admin-next/ | grep -o 'assets/index-[A-Za-z0-9_-]*\.js'
```

Expected: the two hashes are identical; the old chunk returns `404`; the marker
count is at least 1; and the `index-*.js` name in the served HTML matches a file
in `webnext/dist/assets/`. The single strongest assertion is the hash equality —
the served bytes can only come from the embedded tree, so equality proves the
running process is serving the new build.

### Step 6: Confirm the phase-01 fix through the live API

The admin token is the `admin_token` value in the browser's session or local
storage for `/admin-next/`; export it rather than pasting it into a shell history
you keep.

```bash
TOKEN=<admin token>
BASE=http://127.0.0.1:8080/admin/api
for p in 1h 24h; do
  printf '%s chart=%s requests=%s\n' "$p" \
    "$(curl -s -H "X-Admin-Token: $TOKEN" "$BASE/usage/chart?period=$p" | jq 'length')" \
    "$(curl -s -H "X-Admin-Token: $TOKEN" "$BASE/usage/stats?period=$p" | jq '.totalRequests')"
done
```

Expected, before this change: `1h` reported 7 chart buckets while the recent
requests covered 24h. Expected now: `1h` matches `24h` on both lines — 24 buckets
and the same request total — which is what "one value, one window" means. The UI
no longer offers `1h` at all; this check proves the backend no longer answers it
with three different windows.

Measured against the running proxy (pid 87146, binary `1ad4e7f3…`), buckets and
`totalRequests` per period:

| period | buckets | totalRequests |
|---|---|---|
| `1h` | 24 | 2144 |
| `garbage` | 24 | 2144 |
| `24h` | 24 | 2144 |
| `today` | 24 | 1153 |
| `7d` | 7 | 12988 |
| `30d` | 30 | 85811 |
| `60d` | 60 | 197511 |
| `all` | 60 | 242807 |

`1h`, `garbage` and `24h` are now one row instead of three windows, and `all`
charts 60 buckets rather than falling through to 7.

### Step 7: Check the page in a browser

Open `http://127.0.0.1:8080/admin-next/#usage` and confirm: the hero figure with
its five sub-stats, the cost panel with the live indicator, four KPI cards, the
chart panel with its dimension strip, metric toggle, bar series and donut, and the
three tables. Then switch the period to "30 ngày" and confirm the hero, the KPI
row, the chart and all three tables move together — the disagreement phase 01
fixed was exactly that they could not. Leave the tab open past one 15-second
cadence and confirm the "Cập nhật …" timestamp in the header advances.

### Step 8: Commit the artefacts

```bash
cd /Users/van/Tools/OmniProxy
git status --short webnext/ proxy/ web-next/
git add webnext/dist
git commit -F - <<'EOF'
build(webnext): ship the reworked usage dashboard

The client build output is committed because webnext/embed.go embeds it, so the
rebuilt bundle is part of this change. Asset names are content-hashed, so the
previous UsageView chunk is replaced rather than shadowed.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
git push
```

`git status --short` before staging must show the three source trees clean —
phases 01–06 already committed — and only `webnext/dist/` pending. `./omniproxy`
is gitignored and must not appear.

## Todo list

- [ ] Step 1: capture the binary hash, asset names, listening pid
- [ ] Step 2: `npm test`, `tsc -b`, oxlint, `npm run build` all clean
- [ ] Step 3: `./build.sh` produces a different binary containing the marker
- [ ] Step 4: `./restart.sh`, new pid, health endpoint answers
- [ ] Step 5: served chunk hash equals the built chunk hash; old chunk 404s
- [ ] Step 6: `1h` and `24h` agree on bucket count and request total
- [ ] Step 7: the page reads correctly in a browser and the cadence advances
- [ ] Step 8: commit and push `webnext/dist`

## Success criteria

- `npm test` green including the new suites; `tsc -b` and oxlint silent.
- The `omniproxy` hash changed, and the binary contains a string only the new page
  has.
- `shasum` of the served `UsageView-<hash>.js` equals `shasum` of the built file.
- The previously built `UsageView-f3RAeHoC.js` now returns 404.
- `?period=1h` and `?period=24h` return the same bucket count and the same request
  total.
- The page's panels all move together when the period changes.
- `git status` shows no pending source change, and `webnext/dist/` is committed.

## Risk assessment

| Risk | Mitigation |
|---|---|
| `./restart.sh` run without `./build.sh`, so the old embed is re-served and the change looks broken | Key insight 1; step 1 records the before hash and step 3 asserts it changed |
| The binary rebuilt but the job not restarted (pid unchanged) | Step 4 compares the pid against step 1 |
| `webnext/dist/` not written, so the embed picks up the old tree | `strings omniproxy \| grep -c` returns 0; step 3 names the two things to check |
| A stale chunk still served from a browser cache, hiding a good deploy | Step 5 asserts equality on freshly `curl`ed bytes and that the old name 404s, which a cached page cannot fake |
| `./omniproxy` run by hand and the proxy taken down | Key insight 6: restart through `./restart.sh` only |
| A dependency installed to fix a build error | Step 2's `git diff --stat package.json package-lock.json` gate; phases 01–06 install nothing |
| `go build` or `launchctl` fails inside the sandbox and is misread as a code failure | Key insight 5 states the expected denial and the correct way to run it |
| The tracked `dist/` churn makes the commit look unrelated to the feature | Phase 07 commits **only** `webnext/dist`, after the source phases are in |
| `index.html`'s asset names disagree with what was written | Step 5 cross-checks the served HTML's `index-*.js` against `webnext/dist/assets/` |

## Security considerations

- **No secret enters the repo.** `./omniproxy` is gitignored; `data/` is
  gitignored; the admin token is exported in a shell variable for step 6 and never
  committed. `git status --short` before staging must show no `config` or `data`
  path.
- **The deploy does not widen the admin surface.** `webnext/embed.go:50-52` serves
  through `http.FileServer(http.FS(dist))`, and `fs.FS` rejects any path escaping
  its root, so a crafted asset request cannot traverse. `Prefix` (`:39`) is
  `/admin-next/` and Vite's `base` matches, so no asset resolves outside that
  mount.
- **The rebuilt client still authenticates with the same admin token**; no route
  and no auth logic changes in this phase.
- **The API check reads and writes nothing.** Step 6 issues GETs with the operator's
  own token.
- **Rollback is available.** The previous binary snapshots in the repo root
  (`omniproxy.bak-*`) plus the revert of a single commit restore the prior state;
  because `webnext/dist/` is tracked, the previous bundle is recoverable from git
  without a rebuild.

## Next steps

After the deploy, the recorded follow-ups are: adding `requests` and `errors` to
`ChartDataPoint` for the reference's stacked bars, deciding whether `all`/`60d`
should chart wider than 60 day-buckets, and deciding whether `today` becomes
offerable on both admin views — which needs the period state to stop being shared
between them.
