# Thiết kế: Cache thật giảm chi phí + Báo cáo usage cache chính xác

**Ngày:** 2026-07-01
**Trạng thái:** Draft — chờ review
**Phạm vi:** `proxy/kiro.go`, `proxy/cache_tracker.go`, `proxy/response_cache.go` (mới), `proxy/handler.go`, `config/config.go`, `web/`

---

## 1. Bối cảnh & Vấn đề

OmniProxy là proxy đứng trước Kiro (AWS CodeWhisperer/Smithy), phục vụ client định dạng Claude/OpenAI. Hiện tại:

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

## 9. Đánh đổi thiết kế (mỗi quyết định: chọn gì, bỏ gì, giá phải trả)

Mỗi hàng nêu rõ phương án được chọn, phương án bị loại, và **giá phải trả** của lựa chọn.

### 9.1 Match kiểu gì: exact-match vs semantic (embedding)

| Tiêu chí | **Exact-match (CHỌN)** | Semantic/embedding (LOẠI) |
|---|---|---|
| Đúng đắn | Hit ⇒ byte-for-byte giống ⇒ response **luôn hợp lệ** | Similarity ≥ ngưỡng ⇒ có **false positive** (trả sai ngữ cảnh) |
| Chi phí | 1 lần SHA-256, 0 phụ thuộc ngoài | Cần embedding model + vector store (FAISS/Milvus) + tra similarity mỗi request |
| Hit rate | Thấp (chỉ retry/refresh trùng khít) | Cao hơn nhưng **rủi ro trả nhầm** |

**Đánh đổi:** hy sinh hit rate để đổi lấy **đảm bảo đúng tuyệt đối** (không bao giờ trả nhầm câu trả lời). GPTCache README xác nhận semantic dính false positive/negative — với proxy tính tiền, trả nhầm còn tệ hơn miss. Không đưa embedding/vector store vào vì phá vỡ nguyên tắc "một mục đích, ít phụ thuộc".

### 9.2 Phạm vi chia sẻ cache: tách theo apiKey vs global

| | **Tách theo apiKey (CHỌN)** | Global (LOẠI) |
|---|---|---|
| Riêng tư | Không rò rỉ nội dung giữa user | Response user A có thể lộ cho user B |
| Hit rate | Thấp hơn (không chia sẻ chéo) | Cao hơn |

**Đánh đổi:** hy sinh hit rate chéo-user để **không bao giờ rò rỉ dữ liệu**. Với hệ nhiều tenant, an toàn > tiết kiệm. Đây là ràng buộc cứng, không đánh đổi ngược lại.

### 9.3 Cấp cache: response (đầu ra) vs prefix (đầu vào KV)

- **Prefix cache (P2)** là nơi tiết kiệm lớn nhất (cache read 0.1x, hưởng lợi mọi lượt multi-turn) **nhưng** thuộc upstream — proxy chỉ *gợi ý* qua `cache_control`, không tự quản KV được.
- **Response cache (P3)** proxy toàn quyền **nhưng** hit thấp.

**Đánh đổi & hệ quả ưu tiên:** vì phần ăn tiền nhất nằm ngoài tầm kiểm soát trực tiếp, ta đặt P2 (đo + gợi ý prefix) trên P3 (tự cache đầu ra). Không dồn công vào P3 như thể nó là nguồn tiết kiệm chính.

### 9.4 Streaming khi HIT: replay-buffered vs không cache stream

**Chọn:** buffer full text + tool calls trong lúc stream lần đầu, HIT thì replay SSE.
**Giá phải trả:** tốn RAM = tổng kích thước response trong TTL; thêm rủi ro lệch định dạng SSE khi replay. Giảm thiểu bằng cap `MaxEntries` + test khớp byte. **Loại** phương án "chỉ cache non-stream" vì phần lớn traffic là stream — bỏ stream thì P3 gần như vô dụng.

### 9.5 TTL: ngắn (5m) vs dài (1h)

**Chọn mặc định 5 phút.** Đánh đổi: TTL dài → hit cao hơn nhưng **nguy cơ trả nội dung cũ** cao hơn và giữ RAM lâu hơn. 5 phút khớp cửa sổ retry/refresh thực tế (nguồn hit chính) mà vẫn tươi. Chỉnh được qua config cho ai chấp nhận đánh đổi khác.

### 9.6 P1 khi thiếu số thật: fallback simulator vs bỏ trống

**Chọn:** giữ `promptCacheTracker` làm fallback, gắn cờ `estimated=true`.
**Đánh đổi:** phức tạp hơn (2 nhánh) so với "không có số thật thì trả 0". Đổi lại: client cũ vẫn thấy trường cache quen thuộc, và cờ `estimated` phân biệt rõ đo-thật vs ước-lượng nên **không lừa** người đọc. Bỏ trống sẽ gãy client đang dựa vào field đó.

---

## 10. Chứng minh thuật toán (bất biến · độ phức tạp · biên lỗi)

### 10.1 Cache key canonicalization — tính xác định & chống trùng sai

**Thuật toán:** `key = SHA256( apiKeyID ∥ model ∥ canonicalJSON(system, messages, tools, inferenceConfig) )`, dùng `canonicalizeCacheValue` sẵn có (sort key map, loại `cache_control`).

**Bổ đề (xác định):** hai request giống nhau về ngữ nghĩa ⇒ cùng key.
*Chứng minh.* `writeCanonicalJSON` (cache_tracker.go:536) duyệt map theo **key đã sort** (`sort.Strings`, dòng 570), nên thứ tự trường trong JSON gốc không ảnh hưởng chuỗi canonical. Số/bool/string mã hoá bằng `json.Marshal` cố định. Do đó cùng nội dung ⇒ cùng chuỗi canonical ⇒ cùng đầu vào SHA-256 ⇒ cùng digest. ∎

**Bổ đề (chống trùng sai — collision):** xác suất hai request khác nội dung cùng key ≈ 2⁻²⁵⁶ (kháng va chạm SHA-256). Đủ để coi HIT ⇒ nội dung giống. Kèm framing chống nhập nhằng nối chuỗi: `writeHashChunk` (cache_tracker.go:596) ghi `len(chunk) ∥ 0x00 ∥ chunk ∥ 0x00` nên `("ab","c")` ≠ `("a","bc")` — chặn va chạm do ghép biên.

**Vì sao gồm `apiKeyID` trong key:** biến bất biến "không rò rỉ chéo-user" thành **bất biến toán học** — khác apiKey ⇒ khác đầu vào hash ⇒ khác không gian key, không thể HIT chéo. Không dựa vào kiểm tra runtime nào.

**Độ phức tạp:** O(n) theo kích thước request (một lượt canonical + một lượt hash). Không phụ thuộc số entry trong cache.

### 10.2 Dedupe in-flight — đảm bảo "gọi Kiro đúng một lần"

**Cấu trúc:** `inflight map[key]*call`, mỗi `call` có `sync.WaitGroup wg` (Add(1) trước khi thả lock) + slot `result`. Bảo vệ bằng `mu sync.Mutex`.

**Giao thức (một critical section quyết định leader):**
```
mu.Lock()
if c, ok := inflight[key]; ok { mu.Unlock(); c.wg.Wait(); return c.result }  // follower
c := &call{}; c.wg.Add(1); inflight[key] = c; mu.Unlock()                    // leader
c.result = callKiro(...)                                                     // chỉ leader gọi
mu.Lock(); delete(inflight, key); mu.Unlock(); c.wg.Done()
```

**Bất biến 1 (exactly-once):** với một key, tại mọi thời điểm có **tối đa một** `call` trong map. Việc kiểm-tra-và-đặt `inflight[key]` nằm **trọn trong một** vùng khoá `mu`, nên hai goroutine không thể cùng thấy `ok==false` rồi cùng tạo leader. ⇒ chỉ leader gọi `callKiro`. ∎

**Bất biến 2 (không lost-wakeup):** `wg.Add(1)` xảy ra **trước** khi leader thả `mu`; follower chỉ đọc được `c` **sau** khi lấy `mu`, tức sau khi `Add(1)` đã chạy. Vậy không có follower nào `Wait()` trên WaitGroup có counter 0 sớm. Follower luôn được đánh thức bởi `wg.Done()`. ∎

**Bất biến 3 (không giữ khoá khi chờ):** `c.wg.Wait()` gọi **sau** `mu.Unlock()`. Leader hoàn tất không bị chẹn bởi follower đang chờ ⇒ **không deadlock**.

**Biên lỗi:** leader lỗi/panic ⇒ vẫn `delete(inflight,key)` + `wg.Done()` trong `defer`, và `result` mang lỗi. Follower nhận cùng lỗi (không cache lỗi theo 6.4) rồi tự thử lại ở lần sau — không kẹt vĩnh viễn.

### 10.3 Response cache lookup + expiry — không bao giờ trả entry hết hạn

**Bất biến:** `Get(key)` trả HIT **chỉ khi** `now < entry.ExpiresAt`.
*Chứng minh.* Lookup kiểm `entry.ExpiresAt.After(now)` dưới khoá; sai ⇒ coi như MISS và xoá. `pruneExpiredLocked` (mẫu cache_tracker.go:225) là lớp phòng thủ thứ hai chạy định kỳ. Hai lớp đều so cùng mốc `now` lấy một lần ⇒ không có khe "đọc được rồi mới hết hạn". ∎

**Chặn phình RAM (bất biến dung lượng):** sau mỗi `Set`, nếu `len(store) > MaxEntries` thì evict cho tới khi `≤ MaxEntries`. ⇒ bộ nhớ chặn trên bởi `MaxEntries × max_response_size`. Không có đường nào tăng `len(store)` mà bỏ qua kiểm tra này (mọi ghi đi qua `Set`).

**Độ phức tạp:** Get/Set O(1) trung bình (hash map). Prune O(số entry) nhưng nhiếp biên (amortized) vì chỉ chạy theo chu kỳ/khi vượt cap.

### 10.4 Prefix breakpoint (P2) — không vượt giới hạn & không đổi ngữ nghĩa

**Bất biến số breakpoint:** số `cache_control` gắn vào payload ≤ 4 (giới hạn Anthropic). Thuật toán chỉ đặt breakpoint ở các ranh giới cố định `{tools, system, cuối-history}` và đếm, dừng ở 4.

**Bất biến bảo toàn ngữ nghĩa:** `cache_control` là **metadata**; `canonicalizeCacheValue` đã **loại** key `cache_control` (cache_tracker.go:565) khỏi fingerprint. ⇒ thêm/bớt breakpoint **không đổi** cache key P3 và không đổi nội dung ngữ nghĩa gửi model. Bật/tắt P2 do đó trực giao với P1 và P3. ∎

---

## 11. Rủi ro & giới hạn (nói thẳng)

- **P3 hit rate thấp** với hội thoại thật (bằng chứng GPTCache). Chỉ ăn retry/refresh/script lặp. Không kỳ vọng cắt chi phí hội thoại thường.
- **P2 phụ thuộc Kiro** honor cache_control — chưa xác nhận, phải đo trước.
- Nếu Kiro đã tự prefix-cache server-side, phần tiết kiệm lớn nhất **đã xảy ra**; giá trị chính của dự án khi đó là **P1 báo cáo đúng** thay vì bịa số.
- **P1 luôn có giá trị**, rủi ro thấp — nên làm trước và làm chắc.

---

## 12. Thứ tự triển khai

1. **P1** — đọc số thật + fallback simulator (rủi ro thấp, luôn có giá trị, là cảm biến cho P2).
2. **P2 giai đoạn đo** — bật debug, thu log, quyết định honor hay không.
3. **P2 code** (chỉ nếu Kiro honor) — gắn cache_control passthrough.
4. **P3** — response cache + dedupe (mạng an toàn cho request trùng).
