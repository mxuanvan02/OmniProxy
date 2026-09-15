# Phase 06 — Build and verify live

**Priority:** Critical — an unbuilt `webnext/dist` means the Go binary serves the old client.
**Status:** Not started
**Depends on:** phases 01–05
**Blocks:** nothing

## Context links

- `plans/20260915-1622-providers-view/design.md` §2, §7
- `webnext/embed.go`, `build.sh`, `restart.sh`
- `plans/20260915-1622-providers-view/plan.md` — the verified vendor inventory

## Overview

`webnext/dist` is embedded into the Go binary with `//go:embed all:dist`
(`webnext/embed.go:18`) and is **committed to git** (six tracked files, hashed
asset names). The built client therefore only reaches the running proxy after
three steps in this order: rebuild the frontend, rebuild the Go binary, restart
through launchd.

Order matters. `build.sh` is `go build -o omniproxy .` and does not touch the
frontend; running it alone rebuilds a binary around the *old* embedded bundle.
`restart.sh` restarts the launchd job and does not rebuild either.

## Key insights

**1. `./restart.sh` is the only safe way to restart.** Verbatim from the script:
*"TUYET DOI KHONG chay `./omniproxy` bang tay khi da co ban dang chay: main.go
goi CheckAndKillExisting() roi os.Exit(0) — no GIET ban dang chay rot thoat,
KHONG bind cong."* Running the binary by hand kills the live proxy and binds
nothing. The script uses `launchctl kickstart -k`, waits for health 200, and
verifies the job is not respawn-looping.

**2. `restart.sh` needs the sandbox off.** It calls `launchctl` against
`gui/$(id -u)` and `lsof` on the listening socket. Run it with
`dangerouslyDisableSandbox: true`.

**3. An unauthenticated probe cannot verify the new route.** The admin session
gate at `proxy/handler.go:6151` runs **before** the route switch, so both a
registered and an unregistered `/admin/api/pool/health` answer 401 without a
token. There is no way to mint a session token from the shell — that is what the
login flow exists to prevent. Live verification of the authenticated routes is a
browser step, and the automated equivalent is
`TestPoolHealthRouteReturnsSnapshotEnvelope` in phase 02.

**4. The bundle is checkable from disk.** The built `dist/assets/index-*.js`
contains the section's Vietnamese strings, so `grep` proves the new view was
compiled in without needing a session. The hash in the filename also changes,
which is what breaks the browser cache.

**5. The count on the page is a data check, not a code check.** `plan.md`'s
inventory gives 18 distinct hosts plus 7 provider groups — **25 cards**. Without
the trailing-slash normalisation in `hostOf` the same data renders **29**, with
`kiro.pix4k.com`, `sotamodel.net`, `fxqidian.de5.net` and `justwoker.icu` each
appearing twice. That is far easier to spot live than in a fixture.

## Requirements

**Functional**
- `web-next` builds with no type errors and no lint errors.
- `webnext/dist` contains the new bundle and the new strings.
- The Go binary is rebuilt, so the embedded bundle and the new route ship in one
  artifact.
- The service restarts cleanly and reports health 200.
- The page loads at `/admin-next/#providers` with live data.

**Non-functional**
- The full Go test suite passes except the known DNS-dependent failure.
- The full vitest suite passes.
- Nothing is committed that carries a credential.

## Architecture

```
web-next/  npm run build ──► tsc -b ──► vite build ──► webnext/dist/**   (committed)
                                                              │
                                              ./build.sh ─────┤  go:embed all:dist
                                                              ▼
                                                        ./omniproxy  (launchd)
                                                              │
                                              ./restart.sh ───┘  kickstart -k + health poll
                                                              ▼
                                    http://localhost:8080/admin-next/#providers
```

## Related code files

- **Modify:** `webnext/dist/**` — build output (committed; hashed filenames)
- **Not touched:** `omniproxy` — `git ls-files omniproxy` prints nothing, so the
  binary is not tracked and stays out of the commit.
- **Delete:** the previous `webnext/dist/assets/index-*.js` and `.css`, which
  Vite replaces under new hashed names. `webnext/dist/assets/UsageView-*.js` is
  the lazy chunk and also gets a new hash. If Vite leaves the old ones behind,
  `git status` shows the deletions — stage them.

## Implementation steps

### Step 1: Build the frontend

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm run build 2>&1 | tail -20
```

`build` is `tsc -b && vite build`. Expected: `tsc` silent, then Vite reports
`dist/assets/index-<newhash>.js` and one CSS file.

If `tsc -b` fails, stop — it means phase 03's types and phase 04's props
disagree, and no amount of restarting will fix that.

### Step 2: Confirm the new bundle holds the new strings

```bash
cd /Users/van/Tools/OmniProxy
ls -1 webnext/dist/assets/
grep -l 'Nhà cung cấp' webnext/dist/assets/*.js
grep -o 'pool/health' webnext/dist/assets/*.js | head -3
```

Expected: `grep -l` names exactly one JS file — `dist/assets/` holds two, but the
`UsageView` chunk is lazy-loaded and contains none of these strings. `pool/health`
must appear in that same file.

### Step 3: Run the full frontend suite one more time

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -20
npm run lint 2>&1 | tail -10
```

Expected: all suites pass, lint clean. This is the last gate before the artifacts
are rebuilt.

### Step 4: Run the Go suite

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./pool/ ./proxy/ 2>&1 | tail -25
```

Expected: both packages pass, with one tolerated exception —
`TestSearchAdaptersUseNativeContracts/jina-reader` fails on DNS inside the
sandbox. It is pre-existing and unrelated; confirm it is the *only* failure
before continuing.

### Step 5: Rebuild the Go binary

```bash
cd /Users/van/Tools/OmniProxy
./build.sh 2>&1 | tail -10
ls -l omniproxy
```

Expected: no output from the build, and `omniproxy`'s mtime is now.

### Step 6: Restart through launchd

Run with the sandbox disabled — `launchctl` and `lsof` are blocked inside it.

```bash
cd /Users/van/Tools/OmniProxy
./restart.sh
```

Expected, in this order:
- the pre-restart block prints the current PID and `health : HTTP 200`
- `-> launchctl kickstart -k gui/501/com.van.omniproxy`
- the post-restart block prints a **new** PID and `health : HTTP 200`
- `OK — proxy da len, launchd dang quan, health 200.`

If it prints `!! so lan chay tang`, the job is respawn-looping: read
`data/omniproxy.launchd.err.log` before doing anything else.

### Step 7: Verify the unauthenticated surface

```bash
cd /Users/van/Tools/OmniProxy
for url in /admin-next/ /admin-next/index.html /admin/api/pool/health; do
  printf '%s -> ' "$url"
  curl -s -o /dev/null -w '%{http_code}\n' --max-time 8 "http://127.0.0.1:8080$url"
done
curl -s --max-time 8 http://127.0.0.1:8080/admin/api/pool/health
```

Expected: `200`, `200`, `401`, and a body of
`{"error":"Unauthorized"}`.

The 401 is expected for both a registered and an unregistered route — the session
gate runs first. It confirms the route is *behind the gate*, not that it exists.
Existence is proven by phase 02's route test, not here.

### Step 8: Verify the page in a browser

Open `http://localhost:8080/admin-next/#providers` and check, in order:

1. **The nav entry is present** on both the desktop sidebar and, at narrow
   width, the mobile `<select>`. Clicking it changes the URL hash to
   `#providers` — if the hash changes but the page stays on Tổng quan,
   `VALID_SECTIONS` is missing the value.
2. **Reload the page directly at `#providers`.** It must land on the providers
   page, not Tổng quan. This is the check `VALID_SECTIONS` exists for.
3. **The card count is 25**, not 29. Compare against `plan.md`'s inventory: 18
   distinct hosts plus 7 provider groups. More cards means `hostOf` is not
   normalising — look for `kiro.pix4k.com` appearing twice.
4. **`kiro.pix4k.com` shows 6 accounts**, not 4 and 2 in two separate cards.
5. **A vendor with no errors shows `0`**, not `—`, in both the period and
   lifetime error rows.
6. **The period buttons refetch**, and the totals under them change.
7. **The Lỗi gần đây panel** lists each failure under its vendor host — never
   under `External OpenAI`.
8. **Open DevTools → Network, request `/admin/api/pool/health` twice.** The
   second returns 304 from the ETag.

### Step 9: Commit the artifacts

```bash
cd /Users/van/Tools/OmniProxy
git status --short
```

Expected: modified and deleted files under `webnext/dist/` only. `omniproxy` is
not tracked, so it does not appear.

```bash
git add webnext/dist
git commit -F - <<'EOF'
build(web-next): rebuild the embedded admin client for the providers page

webnext/dist is what webnext/embed.go pulls into the binary with go:embed, so
the bundle and the /admin/api/pool/health route it calls only ship together
when the frontend is built before the Go binary.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

Do **not** stage `plans/`, `data/config.json`, or anything under `data/`.

## Todo list

- [ ] Step 1: `npm run build` clean, `tsc -b` silent
- [ ] Step 2: the new strings are in exactly one `dist/assets/*.js`
- [ ] Step 3: full vitest suite and lint pass
- [ ] Step 4: `go test ./pool/ ./proxy/` passes except the known DNS failure
- [ ] Step 5: `./build.sh`, binary mtime updated
- [ ] Step 6: `./restart.sh` reports health 200 and a new PID
- [ ] Step 7: `/admin-next/` 200, `/admin/api/pool/health` 401
- [ ] Step 8: all eight browser checks pass
- [ ] Step 9: commit `webnext/dist`

## Success criteria

- `/admin-next/#providers` loads on a cold reload, from the hash alone.
- The page shows one card per vendor, with no trailing-slash duplicates, and the
  card count matches `plan.md`'s inventory.
- Every figure on a card is a real number or an explicit `—`/`Chưa có dữ liệu`;
  no `NaN`, no `undefined`.
- The pool-health panel names the process start time and states that a restart
  resets it.
- `git status` shows no staged credential material.

## Risk assessment

| Risk | Mitigation |
|---|---|
| Binary rebuilt before the frontend → old bundle embedded | Steps 1 and 5 are ordered and separate; step 2 greps the built file |
| `restart.sh` blocked by the sandbox | `dangerouslyDisableSandbox: true`, named in step 6 |
| `./omniproxy` run by hand, killing the live proxy | Never run it; the in-file warning is quoted in key insight 1 |
| A stale `dist/assets/*.js` left behind and served | Vite clears `outDir` by default; `ls -1 webnext/dist/assets/` in step 2 catches it |
| Committing credentials | `git add webnext/dist` only; step 9 greps nothing from `data/` |
| Assuming a 401 proves the route exists | Key insight 3; existence is proven by phase 02's route test |
| The page shows more cards than expected and it is read as correct | Step 8 check 3 names the expected count and the cause |

## Security considerations

- **No credential enters the build output.** The frontend has no API key, and
  `npm run build` reads no `.env` — the repo has none for this client.
- **The new route stays behind the existing session gate.** Verified live in step
  7: 401 without a token.
- **`data/config.json` is never staged.** It holds every account's credential.
  Step 9 states this explicitly.
- **No external network call is added.** The page fetches three endpoints on the
  same origin.

## Next steps

Back to `plan.md`. If phase 8's browser checks surface a grouping bug, the fix
belongs in `web-next/src/lib/providers.ts` with a test in `providers.test.ts` —
not in the view, and not in the Go layer.
