# Thiết kế: Cache thật giảm chi phí + Báo cáo usage cache chính xác

**Ngày:** 2026-07-01
**Trạng thái:** Draft — chờ review
**Phạm vi:** `proxy/kiro.go`, `proxy/cache_tracker.go`, `proxy/response_cache.go` (mới), `proxy/handler.go`, `config/config.go`, `web/`

---

## 1. Bối cảnh & Vấn đề

SuperKiro là proxy đứng trước Kiro (AWS CodeWhisperer/Smithy), phục vụ client định dạng Claude/OpenAI. Hiện tại:

- `promptCacheTracker` (`proxy/cache_tracker.go`) **mô phỏng** con số cache bằng fingerprint SHA-256 rồi đắp vào field `usage` trả client. Con số này **không liên quan** tới chi phí thật Kiro tính.
- Chi phí thật đến từ `meteringEvent.usage` (`proxy/kiro.go:604-607`) — **do Kiro upstream quyết**, proxy chỉ đọc.
- `updateTokensFromEvent` (`proxy/kiro.go:651`) đã đọc `cacheReadInputTokens`/`cacheWriteInputTokens`/`uncachedInputTokens` từ upstream nhưng **gộp mất** vào `inputTokens`, vứt phần cache đi.
- `KiroPayload` (`proxy/kiro.go:153`) **không mang** `cache_control` — proxy hiện không chủ động bật cache với upstream.

**Mục tiêu:** (1) giảm chi phí token/credit thật; (2) báo cáo usage cache chính xác thay vì bịa số.

---

## 2. Bằng chứng từ research (định hình ưu tiên)

| Nguồn | Kết luận | Hệ quả thiết kế |
|---|---|---|
| Anthropic prompt caching docs | Cache server-side theo prefix + `cache_control`; **cache read = 0.1x** (rẻ 90%); yêu cầu prefix khớp byte-for-byte | Nguồn tiết kiệm lớn nhất là **prefix cache**, thuộc upstream — không phải response cache |
| SGLang RadixAttention (LMSYS) | Prefix/KV cache tự động, luôn bật, không overhead khi miss; workload lợi nhất = **multi-turn chat + few-shot** | Nếu Kiro có prefix cache tự động → tiết kiệm lớn **đã xảy ra**, proxy chỉ cần đọc & báo đúng |
| GPTCache README | Exact-match response cache cho LLM **hit rate thấp**; semantic cache dính false positive/negative | Response cache exact-match **không** là nguồn tiết kiệm chính; chỉ ăn retry/refresh/script lặp |

**Ưu tiên đảo lại theo bằng chứng:** đọc số thật (P1) → điều tra cache_control passthrough (P2) → response cache + dedupe (P3).

---

## 3. Kiến trúc tổng

```
Request → [P3: Response Cache lookup theo (apiKeyID + hash)]
            ├─ HIT  → replay response đã lưu, KHÔNG gọi Kiro (credit=0)
            └─ MISS → [P3: dedupe in-flight] → gọi Kiro
                         └─ [P1: đọc số cache THẬT từ event upstream]
                              ├─ có field cache thật → dùng số thật
                              └─ không có → simulator (đánh dấu estimated)
                         └─ [P2: nếu bật, gắn cache_control vào payload]
                         └─ lưu response cache → trả client
```

Ba cơ chế **độc lập**, bật/tắt riêng, không phụ thuộc lẫn nhau.

---

## 4. P1 — Báo cáo usage chính xác (đọc số thật)

**File:** `proxy/kiro.go`, `proxy/cache_tracker.go`, `proxy/handler.go`

### 4.1 Giữ lại số cache thật từ upstream
- `updateTokensFromEvent` (`kiro.go:651`) hiện gộp `uncached+cacheRead+cacheWrite` thành `inputTokens` rồi vứt chi tiết. **Sửa:** trả thêm struct `realCacheUsage{CacheRead, CacheWrite, Uncached int; Present bool}`.
- `Present=true` khi upstream thực sự bắn ít nhất một field cache.

### 4.2 Callback mới
- Thêm `OnCacheUsage func(realCacheUsage)` vào `KiroStreamCallback`.
- `parseEventStream` gọi callback này khi `Present=true`.

### 4.3 Handler ưu tiên số thật
- `handleClaudeStream`/`handleClaudeNonStream`: nếu nhận được `realCacheUsage.Present` → map thẳng vào `usage` (`cache_read_input_tokens`, `cache_creation_input_tokens`), **bỏ qua** `promptCacheTracker.Compute`.
- Nếu không có số thật → dùng simulator như cũ, nhưng gắn cờ nội bộ `estimated=true` (log/telemetry, không lừa client là số đo thật).

### 4.4 Giữ simulator làm fallback
- **Không xoá** `promptCacheTracker`. Nó là fallback nhánh B (Kiro không bắn field cache).

### 4.5 Cảm biến prefix cache
- Ghi log/metric khi `realCacheUsage.Present` — đây là tín hiệu xác nhận Kiro có prefix cache tự động hay không (trả lời câu hỏi mở của cả dự án).

---

## 5. P2 — Điều tra & (tuỳ chọn) bật cache_control passthrough

**File:** `proxy/kiro.go`, `proxy/handler.go` (sau khi có dữ liệu đo)

### 5.1 Giai đoạn đo (bắt buộc trước khi code)
- Dùng cờ `KIRO_DEBUG_USAGE` (`kiro.go:572`) gửi 2-3 request có prefix trùng, thu log nguyên văn event.
- Xác định: (a) Kiro có bắn `cacheReadInputTokens` không; (b) `meteringEvent.usage` có giảm khi prefix lặp không.

### 5.2 Nhánh kết quả
- **Nếu Kiro honor cache_control / có prefix cache:** thêm `cache_control` breakpoint vào `KiroPayload` khi forward (tại `ClaudeToKiro`) → mở khoá cache read 0.1x. Đây là "cache thật giảm chi phí" đúng nghĩa.
- **Nếu Kiro KHÔNG honor:** dừng P2, không thêm field vô ích. Ghi lại kết luận trong spec.

### 5.3 An toàn
- Chỉ gắn breakpoint ở ranh giới prefix ổn định (tools → system → history), tối đa 4 breakpoint theo giới hạn Anthropic.
- Không đổi ngữ nghĩa request; nếu upstream lỗi vì field lạ → tắt qua cờ.

---

## 6. P3 — Response cache + dedupe in-flight

**File mới:** `proxy/response_cache.go`. **Điểm chèn:** `handler.go:1391` (`handleClaudeStream`), `handler.go:2009` (`handleClaudeNonStream`).

### 6.1 Cache key
- `sha256(apiKeyID + model + canonical(system + messages + tools + inferenceConfig))`.
- **Tách theo `apiKeyID`** → không rò rỉ giữa user. Dùng `canonicalizeCacheValue` sẵn có để chuẩn hoá JSON.

### 6.2 Store
- `map[key]responseCacheEntry` với `{Payload, ExpiresAt, TTL}`. Prune theo mẫu `pruneExpiredLocked` của `promptCacheTracker`.
- Cap `MaxEntries` (LRU đơn giản hoặc prune khi vượt) để chặn phình RAM.

### 6.3 Dedupe in-flight
- `map[key]*inflightCall` với `sync.WaitGroup` + slot kết quả.
- Request thứ 2 trùng key khi request 1 đang bay → chờ, dùng chung kết quả, **không gọi Kiro lần 2**.

### 6.4 Điều kiện cache (mặc định TẮT)
- Chỉ cache khi: cờ `ResponseCacheEnabled=true` **và** không phải response lỗi **và** (`temperature` ≤ ngưỡng hoặc không set — tránh cache nội dung ngẫu nhiên).
- **Không bao giờ** cache khi upstream trả lỗi/timeout.

### 6.5 Streaming
- Buffer full text + tool calls trong lúc stream, lưu vào cache khi hoàn tất.
- Khi HIT: **replay** thành SSE khớp định dạng gốc (message_start → content_block → message_delta → message_stop).

---

## 7. Config & UI

**File:** `config/config.go`, `web/settings.js`, `web/locales/{en,vi,zh}.json`

- `ResponseCacheEnabled bool` (mặc định `false`)
- `ResponseCacheTTL int` (giây, mặc định 300 — dùng chung ý niệm TTL với prompt cache)
- `ResponseCacheMaxEntries int` (mặc định ví dụ 500, như ring buffer usage)
- `CacheControlPassthrough bool` (mặc định `false`, chỉ bật sau khi P2 xác nhận Kiro honor)
- Toggle trong Settings UI + i18n 3 ngôn ngữ.

---

## 8. Test

### P1
- Unit test `updateTokensFromEvent`: event **có** field cache → `realCacheUsage.Present=true`, số đúng; event **không** có → `Present=false`.
- Handler test: có số thật → `usage` dùng số thật, không gọi simulator; không có → fallback simulator + cờ estimated.

### P2
- Test build payload có/không `cache_control` theo cờ; verify tối đa 4 breakpoint; verify không đổi các field khác.

### P3
- Hit/miss/expiry.
- Dedupe: 2 goroutine trùng key → Kiro chỉ được gọi 1 lần (mock `CallKiroAPI`, đếm số lần).
- Stream replay khớp byte định dạng SSE.
- **Không** cache khi: response lỗi / temperature cao / khác `apiKeyID` (test cô lập theo key).

---

## 9. Rủi ro & giới hạn (nói thẳng)

- **P3 hit rate thấp** với hội thoại thật (bằng chứng GPTCache). Chỉ ăn retry/refresh/script lặp. Không kỳ vọng cắt chi phí hội thoại thường.
- **P2 phụ thuộc Kiro** honor cache_control — chưa xác nhận, phải đo trước.
- Nếu Kiro đã tự prefix-cache server-side, phần tiết kiệm lớn nhất **đã xảy ra**; giá trị chính của dự án khi đó là **P1 báo cáo đúng** thay vì bịa số.
- **P1 luôn có giá trị**, rủi ro thấp — nên làm trước và làm chắc.

---

## 10. Thứ tự triển khai

1. **P1** — đọc số thật + fallback simulator (rủi ro thấp, luôn có giá trị, là cảm biến cho P2).
2. **P2 giai đoạn đo** — bật debug, thu log, quyết định honor hay không.
3. **P2 code** (chỉ nếu Kiro honor) — gắn cache_control passthrough.
4. **P3** — response cache + dedupe (mạng an toàn cho request trùng).
