# Phase 07 — Xử lý tồn đọng: SSE Truncation + E1 max_output_tokens

**Ưu tiên:** Vừa · **Trạng thái:** Xong · **Phụ thuộc:** Phase 05, 06

## Tổng quan

Xử lý 2 vấn đề tồn đọng từ phase 05 và 06:
1. **SSE truncation classification** (phase 06 finding): lỗi `external anthropic SSE stream ended before message_stop` rơi vào unclassified failure → cooldown không cần thiết
2. **E1 max_output_tokens** (phase 05 escalated): Responses builder không forward `max_output_tokens` → upstream dùng default thấp hơn mong muốn

Các finding E2-E4 từ phase 05 được defer vì chưa có evidence gây lỗi thực tế.

## Vấn đề 1: SSE Truncation Classification

### Bối cảnh

Sau restart binary (phase 06), log hiện lỗi mới:
```
[AccountFailover] VIBE: unclassified failure for model qwen3.8-max — recording cooldown
(err: external anthropic SSE stream ended before message_stop)
```

Lỗi này xảy ra khi upstream đóng SSE stream gracefully (EOF) nhưng không gửi event `message_stop` terminal. Không phải network error (TCP đóng bình thường), không phải credential fault → rơi vào default branch, ghi cooldown 1 phút cho account khỏe mạnh.

### Phân tích

| Loại lỗi | Nguyên nhân | Hành vi đúng |
|---|---|---|
| Network transient | Socket lỗi giữa chừng | Retry cùng account, rotate without cooldown |
| Kiro truncation | Qwen 200 OK blank body | Short cooldown scoped cho Qwen |
| **SSE truncation** | Upstream đóng stream sớm | **Rotate without cooldown** (mới) |
| Auth/quota | Credential/quota hết | Cooldown dài, lock model |

SSE truncation giống Kiro truncation ở bản chất (upstream-side failure, không liên quan credential) nhưng khác dialect. Pattern đã có sẵn: `IsKiroTruncatedError` check marker "Qwen" + truncation keywords.

### Fix

**File thay đổi:**
- `pool/account.go` (+17 dòng): thêm `IsExternalSSETruncatedError()` với marker "external" + keywords "ended before message_stop" / "ended without assistant output"
- `proxy/account_failover.go` (+9 dòng): thêm branch vào `handleAccountFailure` (rotate without cooldown) và update `waitForPoolRecovery` (allow recovery waves)
- `pool/account_test.go` (+46 dòng): test cả `IsKiroTruncatedError` (trước đây chưa có test) và `IsExternalSSETruncatedError`

**Commit:** `8fe48d8`

### Kiểm chứng

```
$ go test ./pool/ -run "TestIsKiroTruncatedError|TestIsExternalSSETruncatedError" -v -count=1
=== RUN   TestIsKiroTruncatedError (6 subtests)
--- PASS
=== RUN   TestIsExternalSSETruncatedError (6 subtests)
--- PASS
PASS

$ go test ./proxy/ -run "TestHandleAccountFailure|TestWaitForPoolRecovery" -v -count=1
--- PASS (tất cả failover tests)
```

## Vấn đề 2: E1 — max_output_tokens không được forward

### Bối cảnh

Phase 05 audit phát hiện: `responses_types.go:15` khai báo `MaxOutputTokens *int` nhưng builder `kiroPayloadToResponsesRequest` không bao giờ set field này. Client gửi `MaxTokens: 4096` → upstream nhận request không ceiling → dùng default (có thể 1024 hoặc thấp hơn).

### Phân tích

`max_output_tokens` là **safety ceiling**, không phải sampling preference. Khác với `temperature`/`top_p` (opt-in per dialect vì Codex backend reject), ceiling nên **luôn forward** bất kể `ForwardSamplingParams`.

### Fix

**File thay đổi:**
- `proxy/responses_upstream.go` (+7 dòng): thêm block set `body["max_output_tokens"]` khi `InferenceConfig.MaxTokens > 0`, đặt trước sampling params block
- `proxy/responses_upstream_test.go` (+34 dòng): 2 test — forward khi > 0, omit khi = 0

**Commit:** `188a9b0`

### Kiểm chứng

```
$ go test ./proxy/ -run "TestResponsesBuilderForwardsMaxOutputTokens|TestResponsesBuilderOmitsMaxOutputTokensWhenZero" -v -count=1
=== RUN   TestResponsesBuilderForwardsMaxOutputTokens
--- PASS
=== RUN   TestResponsesBuilderOmitsMaxOutputTokensWhenZero
--- PASS
PASS

$ go test ./proxy/ -run "TestResponses" -count=1
PASS (tất cả 18 Responses tests)
```

## Findings deferred (E2-E4)

| # | Vấn đề | Lý do defer |
|---|---|---|
| E2 | Assistant content string vs array | OpenAI chính chủ chấp nhận cả 2; chỉ Azure strict. Chưa có report lỗi |
| E3 | Tool parameters không sanitize | Chỉ ảnh hưởng Responses + Gemini backend. OmniProxy không route qua Gemini |
| E4 | Heuristic "You are " quá rộng | Chưa có report false positive. Redesign cần phase riêng |

**Khi nào revisit:** Khi có user report lỗi cụ thể hoặc khi thêm gateway mới strict hơn.

## So sánh 3 External Dialect (tổng hợp)

| Tiêu chí | Chat Completions | Responses API | Alibaba Cloud Messages |
|---|---|---|---|
| Độ phổ biến | ✅ Cao nhất | ⚠️ Mới (2025) | ✅ Cao trong Alibaba/AWS/GCP |
| Streaming | ✅ Ổn định | ✅ SSE chuẩn | ⚠️ Có lỗi cắt stream (đã fix phase 07) |
| Thinking/Reasoning | ❌ Không có | ✅ Native reasoning | ✅ budget_tokens (fix phase 05) |
| Tool calling | ✅ Chuẩn | ✅ Multi-step | ⚠️ Xung đột thinking (fix phase 05) |
| Spec constraints | Thả lỏng | Trung bình | Chặt chẽ (fix phase 05) |
| max_output_tokens | N/A (dùng max_tokens) | ✅ Fix phase 07 | N/A (dùng max_tokens) |
| Rủi ro vận hành | Thấp | Trung bình | Trung bình |
| Khuyến nghị | Gateway cũ/lạ | OpenAI chính chủ | Alibaba ecosystem |

## File liên quan

- `pool/account.go` — `IsExternalSSETruncatedError()`
- `pool/account_test.go` — test classifiers
- `proxy/account_failover.go` — branch handleAccountFailure + waitForPoolRecovery
- `proxy/responses_upstream.go` — max_output_tokens forwarding
- `proxy/responses_upstream_test.go` — test max_output_tokens
- `proxy/external_anthropic_stream.go:116` — nguồn phát sinh lỗi SSE truncation
