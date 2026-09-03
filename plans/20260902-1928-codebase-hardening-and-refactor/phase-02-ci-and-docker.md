# Phase 02 — CI + Docker hardening

**Context:** [plan.md](plan.md) · source: tests/CI/frontend review (H1, MED 6–7)
**Priority:** HIGH · **Status:** ⬜ pending · **Parallel group:** A (concurrent with 01, 03 — disjoint files)

## Overview

CI verifies nothing Go-related today: `.github/workflows/` holds only `docker.yml` + `issue-validator.yml`. Compilation is checked incidentally by the Dockerfile's `go build`. A broken test or a data race merges silently. Container runs as root with live credentials mounted at `/app/data`, on a floating `alpine:latest` base.

## Key insights

- Repo already builds clean with `GOCACHE=$PWD/.gocache` (needed on this host; `.gocache/` is gitignored). CI runners use the default cache — no override needed.
- `go test -race ./...` passes today (verified, exit 0). Adding it to CI locks that in before phase 01/04/05 land.
- `CGO_ENABLED=0` already set in Dockerfile → static binary → non-root user works without extra libs.
- `modernc.org/sqlite` is pure Go, so no cgo/musl concerns for the non-root switch.

## Requirements

Functional
- Push + PR to `main` runs: `go build ./...`, `go vet ./...`, `go test -race ./...`.
- Docker image runs as non-root, `/app/data` writable by that user.
- Runtime base image pinned to a minor version.

Non-functional
- CI wall time < 5 min (test suite is ~37 s with `-race`).
- No secrets in workflow; no cache poisoning across PRs.

## Architecture

```
push/PR ──▶ ci.yml ──┬─ setup-go 1.25 (+ module cache)
                     ├─ go build ./...
                     ├─ go vet ./...
                     └─ go test -race ./...

docker.yml (unchanged trigger) ──▶ Dockerfile
   stage build : golang:1.25-alpine, CGO_ENABLED=0
   stage run   : alpine:3.21, USER 65532, /app/data chown'd
```

## Related code files

Create
- `.github/workflows/ci.yml`

Modify
- `Dockerfile` — pin runtime base, add non-root `USER`, chown data dir
- `docker-compose.yml` — drop obsolete `version:`, add `healthcheck`, `user:`

Delete — none.

## Implementation steps

1. `.github/workflows/ci.yml`: trigger `push` (branches: main) + `pull_request`; single job `verify` on `ubuntu-latest`; steps = checkout, `actions/setup-go@v5` with `go-version-file: go.mod` and `cache: true`, then build / vet / `test -race`. No `continue-on-error` anywhere.
2. `Dockerfile`: change runtime `FROM alpine:latest` → `alpine:3.21`. After copying the binary, `RUN mkdir -p /app/data && chown -R 65532:65532 /app`, then `USER 65532:65532`.
3. Verify the mounted-volume case: a bind-mounted `./data` owned by the host user may not be writable by uid 65532. Document in `docker-compose.yml` a `user: "${UID:-65532}:${GID:-65532}"` override and note it in the compose comments so an existing deployment does not break on upgrade.
4. `docker-compose.yml`: remove `version: '3.8'`; add `healthcheck` hitting `/health` (unauthenticated per M1, so it needs no credentials); add `user:` per step 3.
5. Build the image locally (`docker build -t omniproxy:ci-test .`) and start it to confirm it can write `/app/data`.

## Todo

- [ ] `ci.yml` created, build+vet+test -race
- [ ] Dockerfile runtime base pinned `alpine:3.21`
- [ ] Dockerfile non-root `USER 65532:65532` + `/app` chown
- [ ] docker-compose: `version:` removed, `healthcheck`, `user:` override
- [ ] Local `docker build` succeeds
- [ ] Container writes `/app/data` as non-root

## Success criteria

- `ci.yml` present; the three Go commands are unconditional job steps.
- `docker build` exits 0; `docker run` starts and the process is not uid 0 (`docker exec … id -u` ≠ 0).
- Container writes `data/config.json` successfully on first boot.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| Existing bind-mounted `./data` owned by host uid → container cannot write, proxy dies on first save | Ship the `user:` override in compose + comment; do not change the default entrypoint behaviour |
| `alpine:3.21` pin drifts stale | Acceptable — pinned minor still gets patch updates on rebuild |
| CI fails on a pre-existing flake | Baseline verified green before this phase; any failure is a real regression |

## Security considerations

- Non-root container limits blast radius of an RCE against the credential store at `/app/data`.
- `/health` used for the healthcheck is intentionally unauthenticated (see M1 in the security report); it leaks nothing.
- Do not add registry credentials or tokens to `ci.yml`; it needs none.

## Next steps

Phase 04 (security) lands after group A. CI added here becomes the gate for 04/05/06.
