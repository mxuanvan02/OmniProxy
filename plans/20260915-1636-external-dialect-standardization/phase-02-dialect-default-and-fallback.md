# Phase 02 — Default `responses` + Fallback 404/405

**Ưu tiên:** Vừa · **Trạng thái:** Chưa làm · **Phụ thuộc:** không (độc lập với Phase 01)

## Bối cảnh

`externalAPIDialect()` (`proxy/external_openai_responses.go:37`) hiện trả `"chat"` trừ khi
`account.ExternalAPIDialect == "responses"`. Đây là mặc định an toàn cho account cũ (D1),
nhưng form add key cũng đang gửi `chat` mặc định (`web/accounts.js:2839`).

Vấn đề thật: gateway resale phần lớn **không nói** nó hỗ trợ dialect nào. Người vận hành
phải đoán. Chọn sai → mọi request 404/405 cho tới khi sửa tay.

## Quyết định đã chốt

- **D1:** account có `ExternalAPIDialect` rỗng ⇒ **giữ `chat`**. Không migrate.
- **D2:** form add key mặc định chọn `responses`.
- **D3:** gặp 404/405 ở `/v1/responses` ⇒ gọi lại bằng chat, và **ghi nhớ** vào account.

## Yêu cầu

### Chức năng
1. `externalAPIDialect()` giữ nguyên hành vi với giá trị rỗng (⇒ `chat`). Không đổi.
2. `dispatchChat` khi dialect là `responses` gọi `CallExternalOpenAIResponses`; nếu trả về
   lỗi "dialect không được hỗ trợ" thì gọi lại `CallExternalOpenAI` **trong cùng request**.
3. Fallback chỉ áp dụng khi **chưa có output nào ra client** (cùng điều kiện retry-safe mà
   `doExternalOpenAIRequest` đang dùng). Đã stream chữ cho client rồi thì không được gọi lại.
4. Sau lần fallback thành công đầu tiên, ghi `ExternalAPIDialect = "chat"` xuống account
   và persist, để các request sau đi thẳng chat.

### Phi chức năng
- Fallback phải **idempotent**: hai request đồng thời cùng account cùng fallback không được
  ghi đè lẫn nhau hỏng config.
- Không được nuốt lỗi thật: 401/402/403 vẫn phải nổi lên cho pool xử lý như hiện tại.

## Thiết kế

### Nhận diện "dialect không được hỗ trợ"

Thêm helper cạnh `externalWAFBlocked` (`proxy/external_openai.go:100`), cùng văn phong:

```go
// externalResponsesDialectUnsupported distinguishes "this gateway has no
// /v1/responses endpoint" from a real request error. 404/405 are the only
// statuses that mean the path itself is absent; a 400 that mentions the
// model is a request problem and must not trigger a silent dialect switch.
func externalResponsesDialectUnsupported(resp *http.Response, body []byte) bool
```

Trả `true` cho: `404`, `405`; và `400` **chỉ khi** body khớp marker đường dẫn
(`unknown url`, `no route`, `not found`, `unsupported path`) — không khớp thì `false`.

### Điểm chèn

`CallExternalOpenAIResponses` (`proxy/external_openai_responses.go:81`) trả lỗi
`HTTP %d from %s: %s` giống `CallExternalOpenAI`. Cần một lỗi **có kiểu** để
`dispatchChat` phân biệt được mà không phải parse chuỗi:

```go
// errExternalDialectUnsupported marks a failure that proves the account's
// configured dialect is absent upstream, so the caller may retry the same
// payload on the chat dialect. Wrapped, so errors.Is still works.
var errExternalDialectUnsupported = errors.New("external upstream does not implement this dialect")
```

`dispatchChat` (`proxy/external_openai.go:1806`):

```go
if externalAPIDialect(account) == "responses" {
    err := CallExternalOpenAIResponses(ctx, account, payload, callback)
    if err == nil || !errors.Is(err, errExternalDialectUnsupported) {
        return err
    }
    // Fallback hợp lệ chỉ khi chưa có gì ra client.
    if callbackProducedOutput(callback) {
        return err
    }
    rememberAccountDialectChat(account) // persist, best-effort
    return CallExternalOpenAI(ctx, account, payload, callback)
}
return CallExternalOpenAI(ctx, account, payload, callback)
```

`callbackProducedOutput` — cần một cờ mà `blankOutputGate` đã có sẵn (`g.meaningful`)
nhưng không expose. Cách rẻ nhất: thêm trường `Produced bool` vào `KiroStreamCallback`
do gate set, đọc lại ở đây. **Không** dùng `OnOutput` trực tiếp vì nó bị gate chặn cho
whitespace-only — đúng ý đồ retry-safety.

### Ghi nhớ dialect

Dùng đúng đường đã có cho việc tương tự — `applyDiscoveredCapabilities`
(`proxy/capability_discovery.go:314`) là tiền lệ: sửa account rồi `config.Save()`.
Viết `rememberAccountDialectChat` theo cùng khuôn, **chỉ ghi khi giá trị thực sự đổi**
để không đập đĩa mỗi request.

## File liên quan

**Sửa:**
- `proxy/external_openai.go` — `dispatchChat` nhánh responses, `externalResponsesDialectUnsupported`
- `proxy/external_openai_responses.go` — trả `errExternalDialectUnsupported` khi 404/405
- `proxy/stream_callback.go` (hoặc nơi định nghĩa `KiroStreamCallback`) — cờ `Produced`

**Tạo:**
- `proxy/external_dialect_fallback_test.go`

## Todo

- [ ] Thêm `errExternalDialectUnsupported` + wrap ở adapter responses
- [ ] Thêm `externalResponsesDialectUnsupported(resp, body)`
- [ ] Thêm cờ "đã có output" trên callback, set từ `blankOutputGate`
- [ ] Nhánh fallback trong `dispatchChat`, có chặn bởi cờ output
- [ ] `rememberAccountDialectChat` + persist, chỉ khi đổi
- [ ] Test: 404 lần đầu → fallback chat thành công → account thành `chat`
- [ ] Test: 401 **không** fallback, lỗi nổi lên nguyên vẹn
- [ ] Test: đã có text ra client rồi thì 404 **không** fallback
- [ ] `go build ./... && go vet ./...`

## Tiêu chí hoàn thành

- Gateway chỉ có chat, account để `responses`: request đầu tự chuyển chat và trả kết quả
  đúng; request thứ hai đi thẳng chat, không thử `/v1/responses` lần nữa.
- Account dialect rỗng: hành vi **byte-identical** với trước phase này (test hồi quy).
- Lỗi auth không bị fallback che.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Fallback che lỗi thật, gây khó debug | Chỉ 404/405 (+400 khớp marker path); log WARN mỗi lần fallback kèm account + status |
| Hai request đồng thời cùng ghi config | Ghi qua `config.Save()` vốn đã khoá; chỉ ghi khi giá trị đổi |
| Gateway trả 404 cho **model** sai, không phải path | Body phải khớp marker path; 404 kèm `model_not_found` ⇒ không fallback |

## Bảo mật

Không đụng credential. Fallback dùng lại đúng `AccessToken` của account, không đổi header.
Log không được chứa key (dùng `accountLabel`).

## Bước tiếp theo

Phase 03 (usage log) độc lập — làm song song được.
