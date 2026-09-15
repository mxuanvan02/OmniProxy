# External OpenAI — Responses API dialect

**Ngày:** 2026-09-15
**Trạng thái:** design + spec đã duyệt; plan ở [`plan.md`](plan.md)
**Phạm vi đã chốt:** phương án A — tối thiểu trước (text + tool call + ảnh), stateless

## 1. Vấn đề

Pool `external_openai` hiện chỉ nói một dialect outbound duy nhất: Chat Completions
(`{BaseURL}/v1/chat/completions`, hoặc path override qua `ChatPath`).

Một số gateway trong pool — ví dụ `token.vietshare.site/codex-api` — có backend là
Codex/ChatGPT subscription, mà backend đó **chỉ** nói Responses API. Khi client gửi
chat completions, gateway phải tự dịch sang Responses nội bộ. Bản dịch đó thường làm
mất `encrypted_content` của reasoning (model phải suy luận lại mỗi turn) và nhồi lại
instruction, nên tốn token hơn so với đi thẳng Responses.

Hiện chưa có cách nào để OmniProxy nói Responses ra ngoài cho các account external.

## 2. Mục tiêu

- Thêm dialect Responses cho account `external_openai`, chọn được khi add key.
- Tái dùng phần dịch request/response đã có ở `external_codex.go`, không copy-paste.
- Không đổi hành vi của account đang chạy (mặc định vẫn là chat).

## 3. Không nằm trong phạm vi

- Mang `encrypted_content` / reasoning items xuyên turn (cần proxy giữ state — phương án B).
- `previous_response_id` / `store: true`.
- Auto-detect dialect. Lần này chọn tay trong form; auto-detect là việc riêng, làm sau.
- Đổi default của bất kỳ account nào đang tồn tại.

## 4. Quyết định đã chốt

| Quyết định | Lý do |
|---|---|
| Mặc định vẫn là chat | Pool là resale gateway không đồng nhất; đổi default làm chết account đang chạy mà không có cảnh báo |
| Chọn dialect bằng field riêng, không tái dùng `ChatPath` | `ChatPath` là *path*, không phải *dialect*; trỏ path thôi vẫn sai body shape |
| Không tự rơi ngầm về chat khi Responses lỗi | Fallback ngầm che lỗi cấu hình và làm chi phí không đo được |
| Tách helper dùng chung thay vì copy-paste | Hai bản sẽ trôi lệch; repo đã có tiền lệ phải sửa 2 nơi |
| Không trừu tượng hóa `CallExternalCodex` bằng transport interface | Hàm đó gắn chặt OAuth refresh, ban detection, reset credits — ngoài phạm vi |

## 5. Thiết kế

### 5.1 Config

Thêm field vào `config.Account`, đặt cạnh `ChatPath` / `ExternalHeaderProfile`
(`config/config.go:136-153`):

```go
// ExternalAPIDialect selects the outbound OpenAI dialect for this
// external account. Empty and "chat" both mean Chat Completions
// ({BaseURL}/v1/chat/completions), which is what every external account
// used before this field existed. "responses" routes the same payload
// through the Responses API shape instead — required by gateways whose
// backend only speaks Responses (e.g. a Codex-subscription reseller).
// It is opt-in per account because the external pool is heterogeneous:
// most resale gateways serve chat only.
ExternalAPIDialect string `json:"externalApiDialect,omitempty"`
```

Setter `SetAccountExternalAPIDialect(id, dialect string) error` theo đúng pattern
`SetAccountExtBillingLimitIsTotal` (`config/config.go:1405`).

> **Sửa đổi sau khi duyệt (2026-09-15).** Setter trên **đã bị bỏ**. Kiểm chứng cho thấy
> `externalApiDialect` là field người dùng nhập, và `apiUpdateAccount` persist bằng cách
> thay toàn bộ record nên một setter gọi giữa chừng sẽ bị ghi đè; setter tương tự được
> viện dẫn ở trên tồn tại cho một đường phát hiện lúc chạy, không phải field người dùng
> nhập. Lý do đầy đủ ở deviation #4 trong [`plan.md`](plan.md). Field được ghi trực tiếp
> lên account literal lúc add key (Task 4.1).

### 5.2 Chọn dialect

Helper trong `proxy/external_openai_responses.go`:

```go
func externalAPIDialect(account *config.Account) string
```

Trả `"responses"` khi field bằng `responses` (không phân biệt hoa thường, có trim),
ngược lại trả `"chat"`. Nil account → `"chat"`.

Field chỉ có hiệu lực với `AuthMethod == "external_openai"`. Account AgentRouter
(`agentrouter` / `external_agentrouter`) dùng chung adapter external nhưng đi qua
`CallExternalAgentRouter` (`external_openai.go:1795`), không đọc field này — nếu
operator set trên account AgentRouter thì bị bỏ qua, không lỗi.

Rẽ nhánh tại `dispatchChat` (`proxy/external_openai.go:1778`), trong nhánh
`isExternalAccount(account)`:

```go
if isExternalAccount(account) {
    if externalAPIDialect(account) == "responses" {
        return CallExternalOpenAIResponses(ctx, account, payload, callback)
    }
    return CallExternalOpenAI(ctx, account, payload, callback)
}
```

### 5.3 Helper dùng chung — `proxy/responses_upstream.go` (mới)

Tách khỏi `external_codex.go` những phần không phụ thuộc Codex:

- Dựng body: từ `kiroPayloadToCodexResponsesRequest` (`external_codex.go:500`).
  Ba hành vi Codex-specific trở thành tham số:
  1. model mặc định (`gpt-5.6-sol`) — external dùng model của payload, không có
     default cứng.
  2. strip `temperature` / `top_p` (`external_codex.go:644-650`) — giữ cho Codex,
     tắt cho external (gateway generic thường nhận).
  3. `codexToolDescription` (`external_codex.go:697`) — chỉ Codex.
- Parse SSE: `parseCodexResponsesSSE` (`external_codex.go:749`) +
  `processCodexSSELine` (`external_codex.go:851`).
- Parse JSON non-stream: `parseCodexResponsesJSON` (`external_codex.go:1004`).

Đầu vào là một struct cấu hình nhỏ (ví dụ `responsesDialectOptions`) thay vì một
`*config.Account`, để phần dùng chung không phải biết về Codex.

`CallExternalCodex` gọi lại helper với options Codex — hành vi không đổi.

### 5.4 Adapter mới — `proxy/external_openai_responses.go`

`CallExternalOpenAIResponses(ctx, account, payload, callback)` song song với
`CallExternalOpenAI` (`external_openai.go:264`), tái dùng nguyên transport của external:

- `setExternalOpenAIHeaders` (`external_openai.go:69`) — giữ nguyên identity headers,
  kể cả profile curl.
- `openAICompatibleEndpoint` (`external_openai.go:1648`) với path `/v1/responses`.
- `doExternalOpenAIRequest` (`external_openai.go:182`) — giữ WAF detect và
  curl-identity fallback (`externalWAFBlocked`, `external_openai.go:100`).
- `blankOutputGate` (`external_openai.go:1080`) — giữ nguyên hành vi phát hiện
  stream 200 rỗng.
- `sseIdleWatchdog` như đường chat.

Khác biệt so với `CallExternalCodex` (`external_codex.go:291`): không OAuth refresh,
không `chatgpt-account-id`, không ban detection, không reset credits, URL lấy từ
`account.BaseURL` chứ không phải `chatgpt.com/backend-api/codex`.

### 5.5 UI

`web/accounts.js` (form add/edit account) + `web/locales/*`: dropdown

- `Chat Completions (mặc định)`
- `Responses API`

Chỉ hiện với account `external_openai`. Field gửi lên là `externalApiDialect`.

## 6. Luồng dữ liệu

```
client (Claude/OpenAI)  →  handler  →  KiroPayload
                                          │
                                   dispatchChat
                                          │
                        externalAPIDialect(account)
                          │                      │
                       "chat"               "responses"
                          │                      │
              CallExternalOpenAI    CallExternalOpenAIResponses
                          │                      │
              kiroPayloadToOpenAIRequest   responses_upstream (build)
                          │                      │
              parseExternalOpenAISSE      responses_upstream (parse SSE)
                          │                      │
                          └──── KiroStreamCallback ────┘
```

`KiroStreamCallback` là điểm hội tụ — mọi handler Claude/OpenAI phía sau không đổi.

## 7. Xử lý lỗi

- Responses trả 404/405 hoặc lỗi shape → trả lỗi rõ ràng cho client, log đủ để biết
  là sai dialect. **Không** tự thử lại bằng chat.
- WAF block và curl-identity fallback giữ nguyên như đường chat.
- Stream 200 rỗng → `blankOutputGate` bắt như đường chat.
- Thiếu `account.BaseURL` → lỗi cấu hình, không gọi mạng.

## 8. Kiểm thử

**Unit — chống regression khi refactor:**
- Với cùng một payload, body do helper dùng chung sinh ra khi bật options Codex phải
  **byte-identical** với body mà `kiroPayloadToCodexResponsesRequest` sinh ra trước
  refactor. Đây là test chặn rủi ro chính.
- `externalAPIDialect`: nil, rỗng, `chat`, `responses`, hoa thường, có khoảng trắng.

**Unit — dialect mới:**
- payload → Responses body: `input` items, `function_call` / `function_call_output`,
  `tools` shape phẳng, `instructions` tách từ priming pair, ảnh trong `codeMessageContent`.
- Responses SSE → callback: text delta, tool call, `response.completed` với
  `cached_tokens` (tham chiếu `proxy/codex_cache_test.go`).
- JSON non-stream → callback.

**Integration:**
- `httptest` server giả gateway Responses: kiểm path `/v1/responses`, header identity,
  body shape, stream.

**Hồi quy:** `go test ./proxy/` đầy đủ, chú ý `external_codex_test.go`,
`codex_cache_test.go`, `codex_image_model_test.go`, `responses_handler_test.go`.

**Lưu ý môi trường:** test bind port sẽ panic trong sandbox — cần
`sandbox.network.allowLocalBinding`.

## 9. Rủi ro

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Refactor `external_codex.go` gây regression Codex | Cao | Test byte-identical ở §8; chạy full suite codex trước khi merge |
| Gateway trả Responses event không chuẩn (thiếu `response.completed`) | Vừa | Đã có blank-output gate; thêm test cho stream thiếu event |
| Model ID không được gateway chấp nhận ở Responses như ở chat | Vừa | `resolveExternalModelID` giữ nguyên; lỗi trả rõ ràng |
| `temperature`/`top_p` gửi tới gateway không nhận | Thấp | Options external bật gửi; nếu gateway từ chối thì xử lý ở tầng lỗi |

## 10. Ước lượng

~1.5 ngày. Chia theo thứ tự: helper dùng chung + test byte-identical trước (rủi ro cao
nhất, làm trước), rồi adapter, rồi UI.
