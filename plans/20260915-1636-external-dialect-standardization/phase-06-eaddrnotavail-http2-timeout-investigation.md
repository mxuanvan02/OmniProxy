# Phase 06 — Điều tra EADDRNOTAVAIL / http2 header timeout

**Ưu tiên:** Chưa rõ → **Thấp (đã fix)** · **Trạng thái:** Xong · **Phụ thuộc:** không

## Bối cảnh

Plan gốc ghi nhận từ bằng chứng đo trực tiếp (2026-09-15): account `TAINGUYENVIBE40KTQ` @ apiforcode có 4 lần failover — 2× `read: can't assign requested address`, 2× `http2: timeout awaiting response headers` → `recording cooldown`. Nghi vấn ban đầu: lỗi mạng ở tầng upstream hoặc local stack, chưa rõ root cause và cách xử lý.

Phase này điều tra: hai lỗi này là gì, code hiện tại xử lý thế nào, có bug không, và cần làm gì thêm.

## Kết luận

**Cả hai lỗi đã được fix đúng trong commit `0036685` (2026-09-15 15:48).** Binary đã build lúc 21:02 cùng ngày nhưng **chưa restart** — fix chưa chạy trên production. Không có action code nào cần thêm; chỉ cần restart binary để fix có hiệu lực.

## Phân tích chi tiết

### Lỗi 1: `read: can't assign requested address` (EADDRNOTAVAIL)

**Root cause:** Local TCP stack cố đọc từ một socket đã bị teardown. Xảy ra khi:
- Network drop/reconnect (WiFi switching, VPN toggle)
- macOS teardown pooled connections sau sleep/wake
- Hai request reuse cùng dead socket trong cùng giây

**Tính chất:** Per-request transport blip, KHÔNG phải account fault. Mọi account đến cùng endpoint qua cùng local stack đều bị như nhau.

**Fix:** `isTransientNetworkError()` (`account_failover.go:184`) nhận diện string `"can't assign requested address"` → `handleAccountFailure` route vào branch `isNetworkError` (line 346-351) → **"rotating without cooldown"**, không record error vào pool.

### Lỗi 2: `http2: timeout awaiting response headers`

**Root cause:** Upstream chấp nhận connection nhưng không gửi response headers trong thời gian `ResponseHeaderTimeout`. Nguyên nhân có thể là:
- Upstream overloaded / slow processing
- HTTP/2 stream multiplexing stall
- Middlebox/proxy buffering
- Network path congestion

**Tính chất:** Per-request transient, KHÔNG phải credential/account fault. Account vẫn valid, chỉ request này stalled.

**Fix:** `isTransientNetworkError()` (`account_failover.go:178`) nhận diện string `"timeout awaiting response headers"` → cùng path rotate without cooldown.

### Bằng chứng log

**Trước fix (7 occurrences, tất cả rơi vào `unclassified failure — recording cooldown`):**

| Thời gian | Account | Model | Lỗi |
|---|---|---|---|
| 2026/09/13 | (1 entry) | — | — |
| 2026/09/14 08:47 | CodexRegistry | — | EADDRNOTAVAIL |
| 2026/09/14 09:47 | SOTA MINH | claude-opus-5 | EADDRNOTAVAIL |
| 2026/09/15 13:59 | TAINGUYENVIBE40KTQ | qwen3.8-max | EADDRNOTAVAIL |
| 2026/09/15 13:59 | TAINGUYENVIBE40KTQ | deepseek-v4-pro-0813 | EADDRNOTAVAIL |
| 2026/09/15 14:11 | TAINGUYENVIBE40KTQ | deepseek-v4-pro-0813 | http2 timeout |
| 2026/09/15 14:11 | TAINGUYENVIBE40KTQ | qwen3.8-max | http2 timeout |

**Sau fix (commit 15:48, binary build 21:02):** Không có entry nào — binary chưa restart nên fix chưa chạy.

### Tác động của bug trước fix

Mỗi lần `unclassified failure — recording cooldown`:
1. Account bị model-lock cooldown (ngắn, nhưng tích lũy)
2. 3 strikes → 1 phút park
3. Pool nhỏ + nhiều transient → cascade exhaust → "no account found" abort
4. Client thấy assistant dừng mid-answer

Với 7 occurrences trong 3 ngày, tác động thấp nhưng real. Fix sẽ loại bỏ hoàn toàn.

## Code review

### `isTransientNetworkError` (`account_failover.go:161-186`)

```go
func isTransientNetworkError(msg string) bool {
    lower := strings.ToLower(msg)
    return strings.Contains(lower, "connection reset") ||
        strings.Contains(lower, "broken pipe") ||
        strings.Contains(lower, "eof") ||
        strings.Contains(lower, "i/o timeout") ||
        strings.Contains(lower, "timeout exceeded") ||
        strings.Contains(lower, "client.timeout") ||
        strings.Contains(lower, "context deadline exceeded") ||
        strings.Contains(lower, "stream idle timeout") ||
        strings.Contains(lower, "timeout awaiting response headers") ||  // ← fix
        strings.Contains(lower, "can't assign requested address") ||     // ← fix
        isTransientHTTP2StreamReset(lower)
}
```

Comment giải thích rõ ràng tại sao mỗi marker được thêm (line 171-185). Code đúng, test cover (`account_failover_test.go:33-35`).

### `handleAccountFailure` routing (`account_failover.go:346-351`)

```go
case isNetworkError(errMsg):
    // Network errors affect all accounts equally when the gateway is down.
    // Do NOT model-lock — just rotate to the next account.
    logger.Debugf("[AccountFailover] Network error for %s: %v — rotating without cooldown", ...)
```

Đúng semantics: network error = rotate, không cooldown, không disable.

## Việc cần làm

| # | Action | Status | Ghi chú |
|---|---|---|---|
| 1 | ~~Fix code~~ | ✅ Done | Commit `0036685` |
| 2 | ~~Build binary~~ | ✅ Done | 2026-09-15 21:02 |
| 3 | **Restart binary** | ⏳ Pending | `restart.sh` hoặc launchctl kick |
| 4 | Verify post-fix log | ⏳ Pending | Tìm `"rotating without cooldown"` thay vì `"unclassified failure"` |

**Không cần code change nào thêm.** Phase này là investigation thuần túy; kết quả là xác nhận fix đã có sẵn và identify gap deploy.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Restart gây downtime ngắn | OmniProxy restart < 1s; client retry tự phục hồi |
| Fix không cover variant lỗi mới | String matching đủ rộng; nếu variant mới xuất hiện, log sẽ hiện `unclassified` → detect ngay |
| Binary cũ vẫn chạy sau build | Đúng là vấn đề hiện tại; cần restart |

## Bảo mật

Không đổi risk profile. Investigation thuần read-only; không đụng credential hay auth flow.

## Bước tiếp theo

- **Immediate:** Restart binary (`./restart.sh` hoặc `launchctl kick -k system/com.omniproxy`) ✅ **Đã làm 2026-09-16 08:39**
- **Verify:** Grep log sau restart tìm `"rotating without cooldown"` cho EADDRNOTAVAIL/http2 timeout → ✅ Không có recurrence nào sau restart (hai lỗi chưa tái xuất hiện)
- **Monitor:** Nếu variant mới xuất hiện (string khác), thêm vào `isTransientNetworkError`

## Finding mới post-restart (ngoài phạm vi phase 06 gốc)

Sau restart, log hiện một lỗi **khác** đang rơi vào `unclassified failure — recording cooldown`:

```
[AccountFailover] VIBE: unclassified failure for model qwen3.8-max — recording cooldown
(err: external anthropic SSE stream ended before message_stop)
```

**Phân tích:**
- Error source: `external_anthropic_stream.go:116` — stream kết thúc mà không nhận được event `message_stop` terminal
- `IsKiroTruncatedError` cố ý exclude external errors (chỉ match string chứa "kiro") → lỗi này không được cover
- Khác với Kiro truncation (200 OK blank body): đây là stream bị cắt giữa chừng, có thể do network drop hoặc upstream reset
- Hiện tại mỗi lần xảy ra → model-lock cooldown cho account VIBE

**Khuyến nghị:** Tạo phase riêng hoặc ticket để phân loại đúng. Hai lựa chọn:
1. Coi là transient network error → rotate without cooldown (nếu nguyên nhân chính là network)
2. Coi là external-truncated error tương tự Kiro → short cooldown scoped cho Alibaba Cloud dialect (nếu nguyên nhân chính là upstream)

Cần thêm data point (tần suất, correlation với network events) trước khi quyết định.
