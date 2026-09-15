# Phase 01 — Anthropic External Adapter

**Ưu tiên:** Cao · **Trạng thái:** Xong (2026-09-15) · **Phụ thuộc:** không

## Bối cảnh

`external_openai` nói được 2 dialect ra upstream: chat-completions (`CallExternalOpenAI`) và
Responses (`CallExternalOpenAIResponses`). Cả hai đều qua `KiroPayload` làm trung gian:
`ClaudeToKiro`/`OpenAIToKiro` dựng payload, adapter dịch payload sang wire shape của upstream,
rồi parse response trả về qua `KiroStreamCallback`.

Một số gateway resale chỉ phục vụ `POST /v1/messages` (Anthropic Messages API) — thường là
hàng xóm bán lại Claude. Hiện không có đường nào tới chúng.

## Yêu cầu

### Chức năng
1. `ExternalAPIDialect == "anthropic"` ⇒ `dispatchChat` gọi `CallExternalAnthropic`.
2. Request gửi lên là Anthropic Messages API hợp lệ: `POST {BaseURL}{AnthropicPath||/v1/messages}`.
3. Header: `content-type: application/json`, `anthropic-version: 2023-06-01`, **cả**
   `x-api-key: <key>` **và** `Authorization: Bearer <key>` (D5).
4. Response (SSE và non-SSE) parse được thành `KiroStreamCallback`: text, thinking, tool_use,
   stop_reason, usage.
5. `stop_reason` map sang vocabulary nội bộ: `end_turn`→`end_turn`, `max_tokens`→`max_tokens`,
   `tool_use`→`tool_use`, `stop_sequence`→`end_turn`.
6. Giữ nguyên các bảo vệ đang có của external pool: `blankOutputGate`, WAF/transient-401 retry
   qua `doExternalOpenAIRequest`, `restoreToolName`/`restoreCallbackToolNames`.

### Phi chức năng
- Không đổi hành vi 2 dialect kia.
- File mới dưới 200 dòng; phần parse SSE tách riêng nếu vượt.

## Thiết kế

### File mới

**`proxy/external_anthropic.go`** — selection + transport + adapter:
- `externalAnthropicPath(account) string` — override `AnthropicPath`, mặc định `/v1/messages`.
- `setExternalAnthropicHeaders(req, account, apiKey)` — D5, tôn trọng `ExternalHeaderProfile`.
- `CallExternalAnthropic(ctx, account, payload, callback) error`.

**`proxy/external_anthropic_translate.go`** — thuần dịch, không I/O:
- `kiroPayloadToAnthropicRequest(payload, account) (map[string]any, error)`
- `parseExternalAnthropicSSE(io.Reader, *KiroStreamCallback) error`
- `parseExternalAnthropicJSON(io.Reader, *KiroStreamCallback) error`

Tách đôi vì lý do đã dùng cho `responses_upstream.go`: phần dịch test được không cần mạng,
và `parseResponsesSSE` là tiền lệ trực tiếp.

### Dịch request: `KiroPayload` → Anthropic Messages

Đây là **nghịch đảo của `ClaudeToKiro`**. Các điểm phải xử lý:

| Nguồn (KiroPayload) | Đích (Anthropic) |
|---|---|
| hệ priming `history[0] user(systemPrompt)` + `history[1] assistant("I will follow…")` | `system` (string, top-level) |
| `AssistantResponseMessage.ToolUses[]` | `content[]` block `tool_use` |
| `UserInputMessageContext.ToolResults[]` | `content[]` block `tool_result` (đứng **trước** text trong cùng turn) |
| `CurrentMessage.UserInputMessageContext.Tools[]` | `tools[]` với `input_schema` |
| `ToolSpecification.Name` (đã sanitize) | `restoreToolName(payload, name)` |
| `Images[]` | block `image` với `source.type=base64` |
| `InferenceConfig.MaxTokens` | `max_tokens` (**bắt buộc** trong Anthropic API) |
| `InferenceConfig.ReasoningEffort` | `thinking.budget_tokens` nếu bật |

Hai điểm dễ sai, phải có test riêng:
- **`max_tokens` bắt buộc.** Thiếu thì Anthropic API trả 400. Khi `InferenceConfig` nil hoặc
  `MaxTokens == 0`, phải chèn một mặc định hợp lý (đề xuất 8192) thay vì bỏ trống.
- **Thứ tự block trong turn user.** `tool_result` phải đứng trước `text`; API trả 400 nếu
  text đứng trước. `kiroPayloadToOpenAIRequest` không có ràng buộc này nên không copy nguyên.

### Parse response

SSE event cần nhận: `message_start` (usage.input_tokens), `content_block_start`
(`text`/`thinking`/`tool_use` — tool_use mang `id`+`name`), `content_block_delta`
(`text_delta`→OnText(false), `thinking_delta`→OnText(true), `input_json_delta`→tích luỹ
`partial_json`), `content_block_stop` (emit tool_use đã tích luỹ), `message_delta`
(`stop_reason` + `usage.output_tokens`), `message_stop`, `error`.

Tái dùng nguyên tắc đã có ở `parseExternalOpenAISSE` (`proxy/external_openai.go:856`):
- `blankOutputGate` bọc callback để turn rỗng vẫn retry được.
- Tool call **tích luỹ rồi emit một lần** khi block đóng, không emit từng delta.
- Stream kết thúc mà chưa thấy terminal ⇒ lỗi `"… stream ended before message_stop"`.
- Bọc `sseIdleWatchdog` y như đường chat.

Sai khác có chủ đích so với bản OpenAI: ở đây tool_use tới **trọn vẹn trong một
`content_block_stop`**, nên không cần map `index → accum` phức tạp như
`externalToolAccum`; một accum đơn theo block index là đủ.

## File liên quan

**Tạo:** `proxy/external_anthropic.go` (162), `proxy/external_anthropic_request.go` (186),
`proxy/external_anthropic_params.go` (156), `proxy/external_anthropic_events.go` (199),
`proxy/external_anthropic_stream.go` (138), `proxy/external_anthropic_json.go` (101)

**Test tạo:** `proxy/external_anthropic_request_test.go`, `proxy/external_anthropic_params_test.go`,
`proxy/external_anthropic_stream_test.go`, `proxy/external_anthropic_adapter_test.go`

**Sửa:**
- `config/config.go` — thêm `AnthropicPath string \`json:"anthropicPath,omitempty"\`` cạnh
  `ChatPath`/`ResponsesPath`
- `proxy/external_openai_responses.go:37` — `externalAPIDialect` nhận thêm `"anthropic"`
- `proxy/external_openai.go:1798` — nhánh `dispatchChat`
- `proxy/handler.go:10708` — nhận `anthropicPath` lúc add key; whitelist dialect
- `proxy/capability_discovery.go` — `discoverableCapabilities` nếu cần khai báo dialect

## Todo

- [x] `config.Account.AnthropicPath` + round-trip JSON test
- [x] `externalAPIDialect` trả `"anthropic"`; giá trị lạ vẫn rơi về `chat`
- [x] `setExternalAnthropicHeaders` (D5) + test header
- [x] `kiroPayloadToAnthropicRequest` — system priming, tools, tool_use, tool_result đúng thứ tự
- [x] `max_tokens` luôn có mặt + test ca `InferenceConfig == nil`
- [x] `parseExternalAnthropicSSE` — text / thinking / tool_use / stop_reason / usage
- [x] `parseExternalAnthropicJSON` cho upstream bỏ qua `stream=true`
- [x] `CallExternalAnthropic` — transport dùng chung `doExternalOpenAIRequest` + gate + watchdog
- [x] Nhánh `dispatchChat`
- [x] `go build ./... && go vet ./... && go test ./proxy/ -run Anthropic`

## Khác so với thiết kế ban đầu

| Thiết kế | Thực tế | Lý do |
|---|---|---|
| 2 file mới (`external_anthropic.go`, `external_anthropic_translate.go`) | 6 file, chia theo trách nhiệm: adapter / request / params / events / stream / json | Giới hạn 200 dòng: gộp lại thì file translate vượt xa. Cách chia theo trục "một file một tầng của pipeline" đọc dễ hơn một file 500 dòng. |
| Trần mặc định 8192 khi `InferenceConfig` nil | Giữ 8192 (`externalAnthropicDefaultMaxTokens`) | Không đổi. |
| `sanitizeExternalToolSchema` tái dùng | Tái dùng + thêm `"type": "object"` ở root | Messages API bắt buộc root object; chat dialect thì không. |
| Phát hiện priming bằng `payload.hasPriming` | Đúng như chốt | Không dò lại chuỗi `"I will follow"` như builder chat. |
| `externalWireModelID` tách riêng | Thêm hàm, `kiroPayloadToOpenAIRequest` dùng chung | Chuỗi phân giải model (requested → Kiro-mapped → "auto") bị lặp ở 3 adapter; một bản sao lệch là bug im lặng. |
| Gate ở `CallExternalAnthropic` | Gate nằm trong 2 parser | Parser Responses dùng chung với Codex nên không gate được ở đó; parser Anthropic chỉ mình adapter này dùng, nên gate tại chỗ đọc stream. |

## Kiểm chứng — kết quả đo (2026-09-15)

```
go build ./...                    # sạch
go vet ./proxy/ ./config/         # sạch
go test ./... -skip TestSearchAdaptersUseNativeContracts
ok  omniproxy/auth 1.061s   ok  omniproxy/cli 2.662s   ok  omniproxy/config 8.986s
ok  omniproxy/logger 2.289s ok  omniproxy/pool 2.216s  ok  omniproxy/proxy 14.405s
```

(`TestSearchAdaptersUseNativeContracts` fail sẵn vì sandbox chặn DNS — không liên quan phase này.)

| Tiêu chí | Test | Kết quả |
|---|---|---|
| Request ra có `system` + `tools` + `tool_result` đúng thứ tự + `max_tokens` | `TestCallExternalAnthropicSendsTheMessagesContract` | Đạt — assert thẳng trên body server nhận |
| Key gửi cả 2 header | cùng test trên | Đạt — `x-api-key` và `Authorization: Bearer` |
| Stream tool_use ⇒ `KiroToolUse` input đã parse | `TestAnthropicSSEEmitsToolCallAtBlockStop` | Đạt |
| JSON hỏng ⇒ không mất call | `TestAnthropicSSEKeepsUnparsableToolArguments` | Đạt — rơi về `{"_raw": ...}` |
| Stream rỗng ⇒ lỗi, không phải thành công | `TestCallExternalAnthropicReportsABlankTurn` | Đạt |
| 2 dialect cũ không đổi hành vi | full suite | Đạt |

Ba phát hiện khi viết test, đã sửa test hoặc code:

1. `tool_result.content` là **string**, không phải block `text` — struct decode trong test ban đầu
   bỏ mất field, test fail vì lý do sai.
2. Ngưỡng `thinking`: `max_tokens <= 4096` thì **không** gửi thinking (API từ chối
   `budget_tokens >= max_tokens`). Test đầu viết sai kỳ vọng, không phải code sai.
3. Stream đứt giữa dòng **vẫn** đã phát text cho client — đúng thiết kế (`OnOutput` chặn retry),
   nhưng test ban đầu kỳ vọng "không phát gì". Đã tách kỳ vọng theo từng ca.

## Tiêu chí hoàn thành

- `httptest` server giả lập gateway Anthropic: request đi ra có `system`, `tools`,
  `tool_result` đúng thứ tự, và `max_tokens` luôn có.
- Stream có tool_use ⇒ client nhận đúng `KiroToolUse` với input đã parse, không phải `_raw`.
- Stream rỗng ⇒ `blankTurnError`, KHÔNG được coi là thành công.
- Hai dialect cũ: test hồi quy vẫn xanh.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Gateway trả JSON lỗi kiểu OpenAI thay vì Anthropic | `extractExternalSSEError` đã bắt `{"error":{"message"}}`; thêm nhánh `{"type":"error"}` |
| `thinking` không được hỗ trợ, gateway 400 | Không gửi `thinking` trừ khi `ReasoningEffort` được set rõ |
| `anthropic-version` bị gateway từ chối | Cho phép override qua header profile; mặc định vẫn gửi |
| Input `tool_use` rỗng (`{}`) | `input_json_delta` không có ⇒ emit `input = {}`, không phải nil |

## Bảo mật

Key gửi ở **hai** header (D5) — chấp nhận được vì cùng một secret, nhưng **log không được
in header**. Dùng `accountLabel`. Không thêm path nào nhận key qua query string.

## Bước tiếp theo

Phase 04 (dropdown) chờ phase này xong. Phase 02 và 03 làm song song được.
