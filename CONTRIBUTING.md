# Contributing to OmniProxy

Thanks for your interest in contributing! This guide covers the development setup, code conventions, and PR process.

## Development Setup

### Prerequisites

- **Go 1.25+** — check with `go version`
- **Git** — for cloning and branching
- **Docker** (optional) — for testing containerised builds

### Get the source

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go mod download
```

### Build & run

```bash
# Build the binary
go build -o omniproxy .

# Run locally (config auto-created at data/config.json)
./omniproxy

# Or use the build script
./build.sh
```

The admin panel is at `http://localhost:8080/admin`. On first run a random admin
password is generated and printed once to stderr — save it. Set `ADMIN_PASSWORD`
to override it; blank and `changeme` are refused.

### Run tests

```bash
# All packages
go test ./...

# Specific package with verbose output
go test ./pool/ -v -run TestStrategy

# With race detector
go test -race ./...
```

### Docker build (multi-arch)

```bash
docker build -t omniproxy:dev .
docker run -d -p 8080:8080 -v $(pwd)/data:/app/data omniproxy:dev
```

## Code Structure

```
auth/      OAuth flows for 12 auth methods (AWS Builder ID, SSO, Social, etc.)
cli/       CLI entry point and interactive menu
config/    Config schema, JSON persistence, KVSettings helpers
pool/      Account pool, round-robin routing, cache warming, routing strategies
proxy/     HTTP handlers, request translators, failover, admin API, web UI
web/       Embedded admin panel (HTML/JS/CSS, i18n locales)
```

### Key concepts

- **AccountPool** (`pool/account.go`) — singleton managing the weighted account slice, cooldowns, model locks, cache stickiness, and routing strategies. `Reload()` rebuilds the slice from config; runtime stats survive reloads via `p.stats`.
- **Handler** (`proxy/handler.go`) — HTTP handlers for `/v1/messages`, `/v1/chat/completions`, `/v1/responses`. Translates between Anthropic/OpenAI formats and the Kiro/Codex upstream.
- **Cache warming** (`pool/account.go` + `proxy/handler.go`) — async background warmup after account rotation, keyed by `hash(instructions)` so all conversations sharing a system prompt benefit from one cache entry.
- **Routing strategies** (`pool/strategy.go`) — `cost-optimized` and `reset-aware` opt-in strategies for 20+ account pools. Round-robin (default) has zero overhead.

## Code Conventions

- **Go style** — follow `gofmt` / `go vet`. No external linter configured; rely on `go vet ./...`.
- **Comments** — document exported functions and non-obvious logic. Explain *why*, not *what*.
- **Error handling** — handle errors at the right boundary. Not every line needs try/catch; look at neighbouring code for the established style.
- **No new dependencies** without justification. The project currently only depends on `google/uuid` and `modernc.org/sqlite`.
- **Tests** — write tests for new pool/proxy logic. Use `t.TempDir()` + `config.Init()` for tests that touch config. See `pool/strategy_test.go` for the pattern.
- **Backward compatibility** — the `superkiro` provider key in config files is kept for backward compatibility. Do not remove these legacy checks.

## Commit Messages

Use conventional commit format:

```
feat: add cost-optimized pool routing strategy
fix: strip temperature/top_p when forwarding to ChatGPT backend
docs: update README with cache warming section
refactor: extract strategy scoring into pool/strategy.go
```

Keep commits focused. One logical change per commit.

## Pull Request Process

1. **Fork & branch** — create a feature branch from `main` (or `dev` if it exists).
2. **Write tests** — especially for pool/proxy logic. Run `go test ./...` before pushing.
3. **Update docs** — if you add a feature, update the relevant README section and `CHANGELOG.md`.
4. **Run checks** — `go build ./... && go test ./... && go vet ./...`
5. **Open PR** — use the PR template, describe what + why, link related issues.
6. **Review** — respond to feedback constructively. We're friendly.

### PR template

PRs should include:

- **Summary** — what changed and why
- **Test plan** — how you verified the change (commands, scenarios)
- **Breaking changes** — if any, call them out explicitly

## Reporting Issues

Use the GitHub issue templates (Bug Report / Feature Request). Include:

- OmniProxy version (or commit SHA)
- Deployment method (Docker / binary / source)
- Steps to reproduce
- Expected vs actual behaviour
- Logs (redact secrets!)

## Fork History

OmniProxy is a fork of [Kiro-Go](https://github.com/Quorinex/Kiro-Go). The upstream licence (MIT) is preserved in [LICENSE](LICENSE).

## Code of Conduct

Be respectful. Disagree on technical merits, not on people. We're all here because we want to make OmniProxy better.
