# Antigravity MITM Proxy — Native Go Implementation

**Created**: 2026-09-16 21:40  
**Status**: Planning  
**Priority**: High  
**Complexity**: Very High (new subsystem)

---

## Context

### User Problem

Google Antigravity accounts are blocked by `VALIDATION_REQUIRED` (device/phone QR verification). User requests architecture that **bypasses this by never calling Google with a non-IDE client** — instead, intercept IDE traffic and serve it from OmniProxy's existing pool (75 accounts, of which 64 are External OpenAI).

**Clarification on "bypass"**: this does not defeat Google's verification. It changes who answers the IDE. Google never receives a request from a non-IDE client, so the `VALIDATION_REQUIRED` path is never entered. The 2 flagged antigravity accounts stay flagged; they are simply not used while MITM is active.

### Verified Prerequisites

- ✅ **Real IDE installed**: `/Applications/Antigravity.app` (Electron / VSCode fork, `app.asar`)
- ✅ **IDE is logged in**: login keychain holds `Antigravity Safe Storage` and `Antigravity IDE Safe Storage` entries. This is required — the MITM intercepts an authenticated session, so the IDE must have completed Google's login legitimately first.
- ✅ **Stdlib CA generation works**: probe test produced a 377-byte CA and a 427-byte leaf for `cloudcode-pa.googleapis.com`; the leaf verifies against the CA. No new Go dependency needed.
- ✅ **Port 8443 free**: nothing listening

### Discovery: 9router MITM Already Exists (But Never Activated Here)

**9router v0.5.45** ships a complete MITM implementation:
- Root CA generation (node-forge)
- TLS listener on `:443`
- DNS hijack via `/etc/hosts` markers (`# 9router antigravity`)
- **136KB protocol translation layer** (Gemini⇄OpenAI, SSE, thoughtSignature, functionCall, safetySettings)
- `fetchRouter` → `http://localhost:20128` → provider node `openai-compatible` → **OmniProxy slot**

**State on this machine**:
- ✅ 9router installed (`/opt/homebrew/bin/9router`)
- ✅ `db.json` has `providerNodes[1]` with `Windsurf Local` → `http://127.0.0.1:8083/v1`
- ✅ `modelAliases` has 25 entries
- ❌ `mitmAlias: {}` (empty)
- ❌ No `rootCA.crt` / `rootCA.key` on disk
- ❌ `/etc/hosts` clean (no markers)
- ❌ Keychain has no "9router" cert
- **Conclusion**: MITM never activated here

### User Decision (AskUserQuestion)

**Architecture**: ✅ **Write native in Go** (not delegate to 9router)
- Reason: Full control, no external dependency at runtime, cleaner integration with OmniProxy pool
- Tradeoff: Must reimplement 136KB translation layer (Gemini⇄OpenAI with agentic features)

**Stub handling**: ✅ **Fix stub to be honest**
- Current: `apiMitmStart` returns "started" but does nothing; `apiMitmStatus` hardcodes `Cert: false`
- Target: Status reflects reality; DNS enable gated on engine presence

---

## Verified Blockers (Empirical)

### Blocker 1: Port 443 Requires Root

```bash
$ go run bind443.go
BIND_443_FAIL: listen tcp 127.0.0.1:443: bind: permission denied
$ id -u
501  # user 'van', not root
```

**Impact**: Cannot bind privileged port as launchd-managed user.

**9router solution**: Spawns entire Node process via `sudo -S sh -c`.

**OmniProxy constraint**: Already runs as `van` under launchd (PID 50001, port 8080).

**Options**:
1. **Use `pfctl` port redirect** (macOS): `443 → 8443`, OmniProxy binds `8443`
2. **Separate privileged helper** (LaunchDaemon): root daemon listens `:443`, pipes to OmniProxy
3. **Manual sudo launch** (9router-style): User runs `sudo omniproxy --mitm` separately

**Recommendation**: **Option 1 (pfctl)** — no extra process, survives reboot via launchd, standard macOS approach.

---

### Blocker 2: /etc/hosts Write Requires Root

```bash
$ ls -la /etc/hosts
-rw-r--r--  1 root  wheel  213 Feb 25  2026 /etc/hosts
$ touch /etc/hosts.tmp
Operation not permitted
```

**Impact**: `addMitmDnsEntry` (handler.go:7867) silently fails; comment says "Try to copy with sudo, fall back to direct write" but neither works as `van`.

**9router solution**: `sudo -S sh -c` wraps the entire write.

**OmniProxy state**: Current code writes to `hostsTmpPath` (`/etc/hosts.tmp`) then calls `atomicRename(hostsTmpPath, hostsFilePath)`, both fail silently.

**Fix required**:
- `addMitmDnsEntry` / `removeMitmDnsEntry` must invoke `sudo tee` or `sudo sh -c`
- Must capture sudo password from UI (already wired: `mitmSudoPass` input in apiCli.js:1219)
- Must return error to caller if write fails (currently void)

---

### Blocker 3: Infinite Loop Risk (CRITICAL)

**Problem**: OmniProxy's antigravity provider calls `cloudcode-pa.googleapis.com` (external_antigravity.go:46). If DNS hijack routes this to `127.0.0.1`, OmniProxy calls **itself** → infinite loop.

**Evidence**:
```go
const antigravityDefaultEndpoint = "https://cloudcode-pa.googleapis.com"
```
All outbound uses default resolver → reads `/etc/hosts`.

**9router solution**: `dns.resolve4` with `8.8.8.8` to **bypass /etc/hosts hijack** for passthrough requests.

**Why "exclude antigravity accounts from pool" does NOT work** (corrected during planning):
Two independent outbound paths reach the hijacked host, and only the first goes through pool selection:
1. `handleOpenAIChat` → pool selection → antigravity provider (excludable)
2. `backgroundRefresh` → `fetchAntigravityModels(account)` (handler.go:2586) — calls the host **directly**, never consults pool availability

Path 2 alone is enough to create the loop. Filtering the pool therefore cannot fix this.

**OmniProxy fix required**: custom `DialContext` that resolves the two cloudcode hosts via an explicit resolver, bypassing `/etc/hosts`.

**Verified injection point** — `buildKiroTransport` (proxy/kiro.go) is the single shared transport factory and currently sets **no** `DialContext`:
```go
t := &http.Transport{
    MaxIdleConns: 100, MaxIdleConnsPerHost: 20,
    IdleConnTimeout: 90 * time.Second,
    ResponseHeaderTimeout: config.GetResponseHeaderTimeout(),
    ForceAttemptHTTP2: true,
}
// no DialContext — safe to inject
```
Both antigravity call sites route through it (`GetClientForProxy`, `GetRestClientForProxy`).

**Scope note**: the IDE calls **both** hosts, so both must be hijacked and both must be bypassed:
- `cloudcode-pa.googleapis.com` (5 refs in `app.asar.unpacked/`, `bin/`)
- `daily-cloudcode-pa.googleapis.com` (1 ref in `app.asar`)

**Recommendation**: **Custom DialContext** — surgical, fixes both paths, preserves all 75 accounts.

---

## Architecture Design

### Request Flow

```
IDE Antigravity
   │ TLS (thinks it's Google)
   ▼
[pfctl redirect :443 → :8443]
   │
OmniProxy MITM listener :8443  ← root CA (stdlib crypto/x509)
   │  intercept ":streamGenerateContent"
   ▼
Gemini→OpenAI translator  ← NEW: 136KB logic port to Go
   │  (contents, functionCall, thoughtSignature, inlineData, safetySettings, SSE)
   ▼
handleOpenAIChat(w, r)  ← EXISTING: reuse entire pool routing
   │
   ▼
pool 74 accounts (64 External OpenAI)
   │
   ▼
OpenAI→Gemini translator  ← NEW: reverse direction
   │  (candidates, parts, usageMetadata, SSE cumulative→delta)
   ▼
IDE receives Gemini-native response
```

**Key insight**: No HTTP round-trip to `:8080/v1/chat/completions`. MITM calls `handleOpenAIChat` **in-process**, inherits all pool logic (routing, combo, alias, failover).

---

### Component Breakdown

#### Phase 0: Stub Honesty Fix (Immediate)
**Files**: `proxy/handler.go`, `web/apiCli.js`

**Changes**:
- `apiMitmStatus` returns `Cert: false, Running: false, DNS: {...}` based on **real state**:
  - Check if `:8443` listener exists (not just `mitmRunning` bool)
  - Check if root CA file exists on disk
  - DNS check already real (reads `/etc/hosts`)
- `apiMitmStart` must **fail with error** if:
  - No CA generated yet
  - No sudo password provided for DNS/pfctl setup
  - Port `:8443` already in use
- `addMitmDnsEntry` / `removeMitmDnsEntry` return `error`, not void
- UI: Show clear error if "Start Server" clicked without CA; disable "Enable DNS" until engine running

**Effort**: 1-2 hours

---

#### Phase 1: Root CA Generation (Stdlib)
**Files**: NEW `proxy/mitm_ca.go`

**Feasibility verified**:
```go
// Probe test: /tmp/ca_probe.go
OK CA_DER_BYTES: 377 LEAF_DER_BYTES: 427
OK leaf verifies against CA for the Google hostname
```

**Implementation**:
- `crypto/ecdsa.GenerateKey(elliptic.P256())` for CA private key
- `x509.CreateCertificate` with `IsCA: true, BasicConstraintsValid: true`
- Persist to `~/.omniproxy/mitm/rootCA.crt` (PEM) + `rootCA.key` (PEM, 0600)
- On-demand leaf cert generation for `cloudcode-pa.googleapis.com` and `daily-cloudcode-pa.googleapis.com`
- Leaf certs cached in-memory by hostname (LRU, 100 entries)
- Admin API: `POST /cli-tools/mitm/ca/generate` → creates CA if missing, returns fingerprint

**Keychain trust**:
- **Manual step** (user must do once): `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.omniproxy/mitm/rootCA.crt`
- UI shows copy-paste command + fingerprint after CA generation
- Status API checks `security find-certificate -c "OmniProxy MITM" /Library/Keychains/System.keychain`

**Effort**: 4-6 hours (CA gen + leaf cache + admin API + tests)

---

#### Phase 2: TLS Listener on :8443 + pfctl Redirect
**Files**: NEW `proxy/mitm_listener.go`, `proxy/mitm_pfctl.go`

**Listener**:
```go
tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{leafCert},  // on-demand via GetCertificate
    GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
        return getOrGenerateLeaf(hello.ServerName)  // cached
    },
    NextProtos: []string{"h2", "http/1.1"},  // IDE uses h2
}
listener, _ := tls.Listen("tcp", "127.0.0.1:8443", tlsConfig)
httpServer := &http.Server{Handler: mitmHandler}
httpServer.Serve(listener)  // stdlib auto-enables h2 when NextProtos includes it
```

**pfctl setup** (macOS port redirect):
```bash
# Requires root, run once on "Start MITM"
echo "rdr pass on lo0 inet proto tcp from any to any port 443 -> 127.0.0.1 port 8443" | sudo pfctl -ef -
```

**Teardown**:
```bash
sudo pfctl -d  # or restore original rules
```

**Alternative** (if pfctl blocked): LaunchDaemon plist at `/Library/LaunchDaemons/com.omniproxy.mitm.plist` that runs `sudo omniproxy --mitm-listener` as root. More complex, survives reboot automatically.

**Effort**: 6-8 hours (listener + pfctl + teardown + error handling + tests)

---

#### Phase 3: DNS Hijack (Real /etc/hosts Write)
**Files**: Modify `proxy/handler.go` (existing `addMitmDnsEntry`)

**Current state**: Code exists but silently fails (no root).

**Fix**:
```go
func addMitmDnsEntry(tool string, sudoPassword string) error {
    // ... build newContent ...
    cmd := exec.Command("sudo", "-S", "tee", hostsFilePath)
    cmd.Stdin = strings.NewReader(sudoPassword + "\n" + newContent)
    cmd.Stdout = io.Discard
    cmd.Stderr = &errBuf
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("failed to write /etc/hosts: %w (stderr: %s)", err, errBuf.String())
    }
    return nil
}
```

**Entries to add**:
```
127.0.0.1 cloudcode-pa.googleapis.com # 9router antigravity
127.0.0.1 daily-cloudcode-pa.googleapis.com # 9router antigravity
```

**Why both**: IDE app.asar references `daily-cloudcode-pa`, but OmniProxy provider uses `cloudcode-pa`. Hijack both to be safe.

**Effort**: 2-3 hours (modify existing funcs, add sudo plumbing, error handling, tests)

---

#### Phase 4: Gemini⇄OpenAI Protocol Translation (THE BIG ONE)
**Files**: NEW `proxy/mitm_gemini_openai.go`, `proxy/mitm_openai_gemini.go`

**Scope** (from 9router analysis):
- **Request** (Gemini→OpenAI):
  - `contents[]` → `messages[]` (role mapping, parts flattening)
  - `systemInstruction` → `messages[0]` with role `system`
  - `generationConfig.thinkingConfig` → OpenAI `reasoning_effort` or custom field
  - `tools[].functionDeclarations` → `tools[].function`
  - `inlineData` (images) → `content[]` multimodal blocks
  - `safetySettings` → drop or map to OpenAI equivalents (if any)
  - `thoughtSignature` → custom metadata field (preserve for reverse translation)

- **Response** (OpenAI→Gemini):
  - `choices[].message` → `candidates[].content.parts[]`
  - `choices[].delta` (SSE) → `candidates[].content.parts[]` (SSE)
  - **SSE semantics**: OpenAI sends **deltas**, Gemini sends **cumulative** — must accumulate
  - `usage.prompt_tokens` → `usageMetadata.promptTokenCount`
  - `usage.completion_tokens` → `usageMetadata.candidatesTokenCount`
  - `tool_calls[]` → `functionCall` parts
  - Inject `thoughtSignature` back if preserved from request

**Reference implementation**: `proxy/translator.go` (Claude⇄KiroPayload, 2420 lines) shows the pattern.

**Effort estimate**: 
- **Conservative**: 40-60 hours (2 weeks part-time)
  - Request translation: 12-16h
  - Response translation: 12-16h
  - SSE streaming + cumulative logic: 8-12h
  - Edge cases (images, tools, thinking): 8-16h
- **Aggressive** (skip non-essential features): 24-32 hours
  - Text-only chat: 8h
  - Basic streaming: 6h
  - Tools/function calling: 10h
  - Drop: images, thinking, safety

**Recommendation**: **Start aggressive** (text + streaming + tools), add features incrementally. Get working end-to-end first.

**Tests**: Golden file tests with real Gemini request/response pairs from `/tmp/ag403.json` and IDE traffic capture.

---

#### Phase 5: MITM Request Handler (Glue)
**Files**: NEW `proxy/mitm_handler.go`

**Design constraint discovered during planning** — the naive buffer-everything approach is **wrong**:

```go
// ❌ DO NOT DO THIS — breaks streaming
buf := &responseBuffer{}
h.handleOpenAIChat(buf, fakeReq)
translateOpenAISSEToGeminiSSE(buf.SSE(), w)   // IDE hangs until generation completes
```

The IDE expects incremental SSE frames. Buffering the full response before translating means the user sees nothing until the entire generation finishes, and long generations look hung.

**Correct design**: a `http.ResponseWriter` wrapper that translates SSE frames **on the fly**, following the existing `streamingPreludeWriter` precedent (proxy/combo.go:140).

**Why that precedent matters** — three concrete requirements it demonstrates:

1. **Must implement `Flush()`**. `handleOpenAIChat` asserts `w.(http.Flusher)` (handler.go:3816) and **bails with "Streaming not supported"** if the assertion fails. There are 9 `http.Flusher` sites in handler.go. A wrapper that only implements `Header/WriteHeader/Write` silently disables streaming.

2. **Buffer the prelude, then pass through**. `streamingPreludeWriter` records into an `httptest.ResponseRecorder` until the first meaningful event appears, then commits and becomes a direct pass-through. The MITM wrapper needs the same two-phase behaviour:
   - **Pre-commit**: buffer, so an OpenAI error JSON (e.g. 503 "No available accounts") can be translated into a proper Gemini error shape rather than leaking OpenAI-format errors to the IDE.
   - **Post-commit**: translate each `data:` frame incrementally, OpenAI delta → Gemini cumulative.

3. **`WriteHeader` must be intercepted** to substitute Gemini-appropriate headers for the OpenAI ones `handleOpenAIChat` sets (`Content-Type: text/event-stream`, etc.).

**Logic**:
```go
func (m *mitmServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !m.shouldIntercept(r) {           // not :streamGenerateContent / :generateContent
        m.passthrough(w, r)              // real Google, via hijack-proof DialContext
        return
    }

    geminiReq, err := readGeminiRequest(r)
    if err != nil { writeGeminiError(w, err); return }

    openaiBody := translateGeminiToOpenAI(&geminiReq)   // Phase 4

    // In-process call — no HTTP round-trip to :8080
    fakeReq := buildOpenAIRequest(r, openaiBody, m.apiKey)

    if geminiReq.Stream {
        tw := newGeminiSSEWriter(w)      // wraps w, implements Flusher,
        h.handleOpenAIChat(tw, fakeReq)  // translates frames on the fly
    } else {
        bw := newGeminiBufferedWriter(w) // non-streaming: buffer is correct here
        h.handleOpenAIChat(bw, fakeReq)
    }
}
```

**Non-streaming path**: buffering *is* correct for `:generateContent` (no SSE), so the two writers differ deliberately.

**Custom DialContext for passthrough** (Blocker 3 fix):
```go
var realGoogleDialer = &net.Dialer{
    Resolver: &net.Resolver{
        PreferGo: true,
        Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
            d := net.Dialer{}
            return d.DialContext(ctx, "udp", "8.8.8.8:53")  // Google DNS, bypasses /etc/hosts
        },
    },
}
```

**Note on HTTP/2**: 9router's MITM uses `http2.createSecureServer` and negotiates `ALPNProtocols:["h2","http/1.1"]`. The Go stdlib equivalent is automatic — `http.Server.Serve` enables h2 when the listener returns `*tls.Conn` with `"h2"` in `NextProtos` (verified via `go doc net/http.Server.Serve`). No `golang.org/x/net/http2` dependency needed.

**Effort**: 8-12 hours (handler + 2 writers + passthrough + error handling)

---

#### Phase 6: Admin API + UI Wiring
**Files**: Modify `proxy/handler.go` (routes), `web/apiCli.js` (UI)

**New endpoints**:
- `POST /cli-tools/mitm/ca/generate` → generates CA, returns fingerprint + trust command
- `GET /cli-tools/mitm/ca/status` → checks CA file + Keychain trust
- `POST /cli-tools/mitm/pfctl/enable` → sets up port redirect (requires sudo)
- `DELETE /cli-tools/mitm/pfctl/disable` → removes redirect

**Modify existing**:
- `POST /cli-tools/mitm/server` → now actually starts listener, requires CA present
- `GET /cli-tools/mitm/status` → returns real state (listener running, CA trusted, DNS active, pfctl active)
- `PATCH /cli-tools/mitm/dns` → accepts `sudoPassword` in body, returns error if write fails

**UI changes** (`web/apiCli.js`):
- "Generate CA" button → calls `/ca/generate`, shows fingerprint + copy-paste trust command
- "Start Server" disabled until CA exists and is trusted
- "Enable DNS" prompts for sudo password (input already exists: `mitmSudoPass`)
- Status panel shows 4 indicators: CA ✅, Listener ✅, DNS ✅, pfctl ✅
- Error messages if any step fails

**Effort**: 6-8 hours (API endpoints + UI state machine + error handling)

---

#### Phase 7: web-next UI (React)
**Files**: NEW `web-next/src/components/MitmView.tsx`

**Scope**: Port `apiCli.js` MITM panel to React with better UX.

**Features**:
- Step-by-step wizard: Generate CA → Trust in Keychain → Enable pfctl → Enable DNS → Start listener
- Real-time status indicators
- Model alias editor (reads/writes `~/.omniproxy/mitm/aliases.json`)
- Log viewer (tail MITM-specific logs)

**Effort**: 12-16 hours (React component + state management + API integration)

**Priority**: Low (legacy `web/apiCli.js` is sufficient for MVP)

---

## Security Considerations

### Root CA Private Key
- Stored at `~/.omniproxy/mitm/rootCA.key` with `0600` permissions
- **Never logged** (use `accountLabel` pattern, not key material)
- **Never sent over API** (only fingerprint exposed)
- Admin API requires session auth (existing `adminSessions.valid()` gate)

### Sudo Password Handling
- Accepted via POST body (not query string — prevents log leakage)
- Used immediately in `exec.Command("sudo", "-S", ...)`, then discarded
- **Never persisted** to disk or config
- UI input type `password` (already correct in apiCli.js:1219)

### TLS Listener Security
- Binds `127.0.0.1:8443` only (loopback, not `0.0.0.0`)
- Leaf certs generated on-demand, cached in-memory only
- No persistent leaf cert storage (regenerate on restart)

### DNS Hijack Scope
- Only hijacks `cloudcode-pa.googleapis.com` and `daily-cloudcode-pa.googleapis.com`
- Uses `# 9router antigravity` marker for clean removal
- Teardown on "Stop Server" removes all markers

### Infinite Loop Prevention
- Custom `DialContext` for passthrough uses `8.8.8.8` resolver
- **Or**: antigravity provider accounts excluded from pool when MITM active (simpler fallback)
- Monitoring: log warning if same request ID seen twice within 1s

---

## Risk Assessment

### High Risk
1. **Protocol translation bugs** (Phase 4):
   - Gemini SSE cumulative vs OpenAI delta semantics
   - Tool call format differences
   - Thinking/reasoning token handling
   - **Mitigation**: Golden file tests, incremental feature rollout, extensive logging

2. **macOS pfctl fragility** (Phase 2):
   - Rules may conflict with user's existing pf config
   - Survives reboot only if persisted to `/etc/pf.anchors/`
   - **Mitigation**: Check existing rules before applying, provide manual teardown command, document in UI

3. **Keychain trust manual step** (Phase 1):
   - User must run `sudo security add-trusted-cert` once
   - If skipped, IDE shows TLS errors
   - **Mitigation**: UI prominently displays command + fingerprint, status check detects untrusted CA

### Medium Risk
4. **Port conflicts**:
   - `:8443` may already be in use
   - **Mitigation**: Configurable port, status check before bind, clear error message

5. **IDE version drift**:
   - Future Antigravity IDE updates may change protocol
   - **Mitigation**: Version detection via User-Agent, graceful degradation (passthrough on unknown)

6. **Google ToS**:
   - This architecture **does not bypass verification** — it replaces the provider entirely
   - Google never sees requests from OmniProxy's antigravity accounts when MITM active
   - **Risk**: If user disables MITM and resumes direct antigravity calls, those 2 accounts remain flagged
   - **Mitigation**: Document clearly; recommend user complete verification on those accounts anyway as backup

### Low Risk
7. **Performance**:
   - In-process translation adds latency
   - **Mitigation**: Benchmark, optimize hot paths (SSE streaming)

8. **Memory**:
   - Leaf cert cache (100 entries) + SSE buffers
   - **Mitigation**: Bounded cache, stream processing (no full body buffering for large requests)

---

## Implementation Order

### MVP (Working End-to-End)
1. **Phase 0**: Stub honesty fix (1-2h) — prevents current dangerous state
2. **Phase 1**: Root CA generation (4-6h) — prerequisite for everything
3. **Phase 2**: TLS listener + pfctl (6-8h) — core infrastructure
4. **Phase 3**: DNS hijack (2-3h) — routing
5. **Phase 4 (aggressive)**: Text-only translation (8h) + basic streaming (6h) — minimum viable protocol
6. **Phase 5**: MITM handler glue (8-12h) — wire it together
7. **Phase 6**: Admin API + legacy UI (6-8h) — user control

**Total MVP**: ~40-50 hours (1-1.5 weeks full-time)

### Post-MVP Enhancements
8. **Phase 4 (full)**: Tools/function calling (10h), images (4h), thinking (4h)
9. **Phase 7**: web-next React UI (12-16h)
10. **Monitoring**: Prometheus metrics, request logging, error tracking

---

## Success Criteria

### Functional
- [ ] IDE Antigravity connects without TLS errors
- [ ] Chat requests routed through OmniProxy pool (not Google)
- [ ] Streaming responses work (SSE)
- [ ] Model aliases respected (`~/.omniproxy/mitm/aliases.json`)
- [ ] "Stop Server" cleanly tears down (listener, DNS, pfctl)
- [ ] Status API reflects real state (no lies)

### Non-Functional
- [ ] No infinite loops (antigravity provider still works when MITM active)
- [ ] Latency overhead < 50ms per request (translation + in-process routing)
- [ ] Memory overhead < 10MB (leaf cert cache + buffers)
- [ ] Survives OmniProxy restart (CA persists, pfctl persists if configured)

### Security
- [ ] Root CA key never logged or exposed via API
- [ ] Sudo password never persisted
- [ ] TLS listener bound to loopback only
- [ ] DNS hijack scoped to Google hosts only

---

## Out of Scope (Deferred)

1. **Windows/Linux support**: macOS-only for MVP (pfctl is macOS-specific)
2. **Automatic Keychain trust**: Requires user interaction by design (security)
3. **Full 9router feature parity**: 9router has Copilot/Kiro/Cursor MITM; OmniProxy MVP is antigravity-only
4. **Quota integration**: `retrieveUserQuota` from 9router not ported (separate feature request)
5. **Persistent request logs**: MITM-specific logging deferred to monitoring phase

---

## Dependencies

### Stdlib Only (No New Go Modules)
- `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand` — CA key generation
- `crypto/x509`, `crypto/x509/pkix` — certificate creation
- `crypto/tls` — TLS listener
- `net/http` — HTTP/2 server (auto-enabled via `NextProtos`)
- `encoding/pem` — certificate serialization
- `os/exec` — sudo invocation for DNS/pfctl
- `net` — custom DialContext for passthrough

### External Tools (Already Present)
- `sudo` — for `/etc/hosts` write and `pfctl`
- `pfctl` — macOS port redirect (built-in)
- `security` — Keychain trust check (built-in)

### Existing OmniProxy Code (Reuse)
- `handleOpenAIChat` (handler.go:5135) — in-process routing
- `OpenAIRequest` / `OpenAIResponse` (translator.go:1119) — request/response structs
- `GetClientForProxy` (kiro.go:73) — HTTP client factory (inject custom DialContext)
- `apiCli.js` MITM panel (web/apiCli.js:1210-1330) — legacy UI (modify, not replace)

---

## Open Questions

1. **pfctl vs LaunchDaemon**: Which approach for privileged listener?
   - pfctl: simpler, no extra process, but rules may conflict
   - LaunchDaemon: cleaner isolation, survives reboot, but more complex setup
   - **Recommendation**: Start with pfctl, add LaunchDaemon as fallback option

2. **Infinite loop prevention**: Custom DialContext vs exclude antigravity accounts?
   - DialContext: surgical, preserves all accounts, but complex
   - Exclude accounts: simple, but reduces pool capacity by 2
   - **Recommendation**: Custom DialContext (already have reference impl from 9router)

3. **Protocol translation scope**: Aggressive (text+stream+tools) vs full (add images+thinking)?
   - Aggressive: 24-32h, covers 90% of IDE usage
   - Full: 40-60h, covers 100%
   - **Recommendation**: Start aggressive, add features based on user feedback

4. **Model alias source**: `~/.omniproxy/mitm/aliases.json` vs 9router's `db.json:modelAliases`?
   - OmniProxy-native: cleaner, no 9router dependency
   - 9router import: user already has 25 aliases configured
   - **Recommendation**: OmniProxy-native, but add one-time import from 9router if `db.json` exists

---

## Next Steps

1. **User approval** of this plan
2. **Phase 0 implementation** (stub honesty fix) — can start immediately, low risk
3. **Phase 1 spike** (CA generation proof-of-concept) — validate stdlib approach
4. **Phase 4 design doc** (Gemini⇄OpenAI translation spec) — detailed field mapping before coding

---

## References

### 9router MITM Source
- `/opt/homebrew/lib/node_modules/9router/app/src/mitm/server.js` (319KB minified)
- Key functions: `fetchRouter`, `pipeTransformedSSE`, `Yd` (antigravity body transformer)
- CA generation: `forge.pki` in `app/.next-cli-build/server/chunks/915.js`

### OmniProxy Existing Code
- `proxy/handler.go:7731-7924` — MITM stub handlers (to be replaced)
- `proxy/external_antigravity.go` — antigravity provider (must not loop)
- `proxy/translator.go` — Claude⇄KiroPayload translation (pattern reference)
- `web/apiCli.js:1210-1330` — legacy MITM UI (to be modified)

### Empirical Tests
- `/tmp/ca_probe.go` — stdlib CA generation ✅
- `/tmp/bind443.go` — privileged port bind ❌ (requires root)
- `/etc/hosts` write test ❌ (requires root)

### User Decision Record
- Architecture: **Native Go** (not 9router delegation)
- Stub handling: **Fix to be honest** (not remove)
- Scope: **MVP first** (text+stream+tools), enhance later
