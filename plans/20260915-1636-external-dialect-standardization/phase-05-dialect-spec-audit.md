# Phase 05 — Audit 3 dialect với spec chính thức

**Ưu tiên:** Vừa · **Trạng thái:** Xong (fix + escalate) · **Phụ thuộc:** Phase 01, 02

## Bối cảnh

Phase 01–04 đã build xong 3 external dialect (chat / responses / Alibaba Cloud Messages) và UI chọn dialect. Nhưng chưa có lần nào đối chiếu wire shape với spec chính thức của từng contract. Phase này đọc code, fetch doc, ghi nhận gap và fix những cái chắc chắn gây 400 mà không đụng shared Codex/Responses code.

## Quyết định đã chốt

- **Chỉ fix bug khu trú trong file Alibaba Cloud-only.** Hai finding nghiêm trọng nhất (thinking + sampling, thinking + forced tool_choice) nằm hoàn toàn trong `external_anthropic_params.go` — không ảnh hưởng Codex hay Responses builder. Fix ngay, thêm test.
- **Escalate findings ở shared builder.** Các vấn đề ở `responses_upstream.go` (ảnh hưởng cả Codex lẫn generic Responses gateway) cần golden update và đánh giá risk trước khi sửa — không làm trong phase này.
- **Không đổi hành vi chat dialect.** Chat là dialect mặc định cho 65 account cũ; bất kỳ thay đổi nào cũng cần regression test rộng hơn. Ghi nhận gap, không fix.
- **Spec sources:** Alibaba Cloud Messages API doc chính thức (`platform.claude.com/docs/en/build-with-claude/thinking`, `extended-thinking`); Azure OpenAI Responses doc (`learn.microsoft.com/.../responses`) vì openai.com trả 403; model typed trong repo (`responses_types.go`) làm bằng chứng nội tại.

## Findings

### ✅ ĐÃ FIX — Dialect Alibaba Cloud Messages

| # | Vấn đề | Spec | Code trước | Fix | Test |
|---|---|---|---|---|---|
| F1 | `thinking` + `temperature` cùng lúc → 400 | Doc chính thức: "temperature and top_k are incompatible with thinking" | `applyAnthropicSamplingParams` set cả hai độc lập | Drop temperature/top_p khi thinking on | `TestAnthropicThinkingDropsSamplingParams` |
| F2 | `thinking` + forced `tool_choice` (`any`/`tool`) → 400 | Doc: "tool_choice any/tool results in an error because these options force tool use, which is incompatible with manual extended thinking" | Builder set tool_choice trước, rồi set thinking mà không check | Forced tool_choice wins (drop thinking); auto/nil giữ nguyên | `TestAnthropicForcedToolChoiceDropsThinking` |
| F3 | `top_p > 1` → 400 | Doc: top_p range [0,1] | Chỉ check `> 0`, không clamp trên | Thêm `&& cfg.TopP <= 1` | `TestAnthropicTopPClampToOne` |

**File sửa:** `proxy/external_anthropic_params.go` (156 → 194 dòng, vẫn dưới 200).
**Test thêm:** 3 test mới trong `proxy/external_anthropic_params_test.go`.
**Risk:** Thấp — chỉ ảnh hưởng dialect Alibaba Cloud, không đụng shared code.

### ⚠️ ESCALATE — Shared Responses Builder (`responses_upstream.go`)

| # | Vấn đề | Spec | Code hiện tại | Risk nếu fix |
|---|---|---|---|---|
| E1 | Không gửi token ceiling | Azure doc: field là `max_output_tokens`; `responses_types.go:15` khai báo `MaxOutputTokens *int` nhưng builder không bao giờ set | Client gửi request không ceiling → upstream dùng default (có thể thấp hơn mong muốn) | Medium — cần thêm field vào `responsesDialectOptions`, update golden Codex |
| E2 | Assistant message content là plain string | Azure doc: assistant content phải là array `[{type:"output_text", text:...}]`; user content chấp nhận plain string | `responses_upstream.go:129` emit `"content": am.Content` (string) | Medium — một số gateway lenient, một số strict; cần test thực tế |
| E3 | Tool parameters không sanitize | Chat dialect gọi `sanitizeExternalToolSchema` (drop non-string enum); Responses builder truyền raw `InputSchema.JSON` | Gemini-compatible gateway reject malformed enum | Low-Medium — chỉ ảnh hưởng gateway dùng Responses + Gemini backend |
| E4 | System prompt heuristic "You are " quá rộng | Bất kỳ user message mở đầu bằng "You are " bị lift thành instructions | False positive: user hỏi "You are a doctor, what do you think?" → mất message | Low — heuristic có sẵn từ trước phase 01, chưa có report |

**Lý do escalate:** Tất cả nằm trong `kiroPayloadToResponsesRequest` — shared giữa Codex (`codexResponsesOptions`) và generic Responses gateway (`externalResponsesOptions`). Fix cần:
1. Update golden test (`testdata/codex_responses_body.golden.json`)
2. Verify Codex backend vẫn accept shape mới
3. Có thể cần feature flag hoặc dialect option riêng

**Khuyến nghị:** Tạo phase riêng (05b hoặc 07) sau khi có test integration với Codex backend thật.

### ℹ️ GHI NHẬN — Chat Dialect (`external_openai.go`)

| # | Vấn đề | Ghi chú |
|---|---|---|
| G1 | Không có audit đầy đủ | Chat là dialect mặc định, đã chạy ổn định từ trước phase 01. Audit sâu cần thời gian và risk cao hơn. |
| G2 | Sampling params không clamp | Chat accept temperature đến 2 (OpenAI spec), nên không cần clamp như Alibaba Cloud. OK. |
| G3 | Tool schema sanitize | Đã có `sanitizeExternalToolSchema` — OK. |

## Kiểm chứng — kết quả đo

```
$ wc -l proxy/external_anthropic_params.go
     194 proxy/external_anthropic_params.go

$ go build ./...
(sạch)

$ go vet ./proxy/
(sạch)

$ go test ./proxy/ -run 'TestAnthropic' -count=1 -v
=== RUN   TestAnthropicThinkingDropsSamplingParams
--- PASS: TestAnthropicThinkingDropsSamplingParams (0.00s)
=== RUN   TestAnthropicForcedToolChoiceDropsThinking
--- PASS: TestAnthropicForcedToolChoiceDropsThinking (0.00s)
=== RUN   TestAnthropicTopPClampToOne
--- PASS: TestAnthropicTopPClampToOne (0.00s)
... (tất cả 18 test Alibaba Cloud pass)
PASS
ok  	omniproxy/proxy	0.369s
```

## File liên quan

**Sửa:**
- `proxy/external_anthropic_params.go` (+38 dòng) — refactor `applyAnthropicSamplingParams`: drop sampling khi thinking on, drop thinking khi forced tool_choice, clamp top_p ≤ 1. Thêm helpers `externalAnthropicMaxTokens`, `anthropicToolChoiceForced`.
- `proxy/external_anthropic_params_test.go` (+52 dòng) — 3 test mới cover F1, F2, F3.

**Tạo:**
- `phase-05-dialect-spec-audit.md` (doc này)

**Không sửa:**
- `proxy/responses_upstream.go` — findings E1-E4 escalated
- `proxy/external_openai.go` — chat dialect giữ nguyên
- Golden files — cần phase riêng

## Todo

- [x] Đọc code 3 dialect (chat, responses, Alibaba Cloud)
- [x] Fetch spec chính thức (Alibaba Cloud Messages, OpenAI Responses)
- [x] Xác minh ràng buộc thinking + sampling, thinking + tool_choice
- [x] Fix F1: drop temperature/top_p khi thinking on
- [x] Fix F2: forced tool_choice wins over thinking
- [x] Fix F3: clamp top_p ≤ 1
- [x] Thêm 3 test mới
- [x] Build/vet/test xanh
- [x] Viết phase-05 doc, cập nhật plan.md
- [ ] Escalate E1-E4 (phase riêng)

## Tiêu chí hoàn thành

- Hai bug 400-chắc-chắn ở dialect Alibaba Cloud được fix và test cover.
- Findings ở shared builder được ghi nhận rõ ràng với risk assessment.
- Không phá Codex hay chat dialect.
- Doc ghi lại spec sources để future maintainer verify.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Fix thinking/sampling làm mất reasoning depth khi client gửi cả hai | Đúng spec: API reject cả hai cùng lúc. Drop sampling giữ thinking — reasoning depth quan trọng hơn temperature fine-tuning. |
| Forced tool_choice wins làm mất thinking | Đúng spec: API reject cả hai. Forced call quan trọng hơn — drop thinking chỉ mất reasoning depth, drop forced call phá tool loop. |
| TopP clamp thay đổi hành vi client đang gửi >1 | Client gửi >1 đã bị API reject 400. Clamp = graceful degradation thay vì hard fail. |
| Escalated findings bị quên | Ghi rõ trong doc + plan.md Task Index. |

## Bảo mật

Không đổi risk profile. Fixes chỉ ảnh hưởng parameter validation, không đụng credential hay auth flow.

## Bước tiếp theo

- **Phase 05b (hoặc 07):** Fix escalated findings E1-E4 ở shared Responses builder. Cần golden update + Codex integration test.
- **Phase 06:** Điều tra `EADDRNOTAVAIL` / `http2` header timeout (độc lập).
