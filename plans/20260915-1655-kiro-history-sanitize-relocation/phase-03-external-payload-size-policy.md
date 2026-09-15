# Phase 03 — Chính sách kích thước payload cho external

**Ưu tiên:** Thấp · **Trạng thái:** Đã làm — chưa commit · **Phụ thuộc:** phase 02
**Loại:** quyết định + thực thi nhỏ

## Bối cảnh

Hôm nay `truncatePayloadToLimit` (`translator.go:1770`) chạy trong **cả hai** translator,
nên **mọi** account — kể cả external — đều được cắt về `maxPayloadBytes = 900KB`
(`translator.go:64`). Nhưng nó cắt trên history **đã sanitize**, tức bản đã co ~1000 lần
trong fixture đo được, nên thực tế **gần như không bao giờ fire** cho external.

Phase 02 bỏ lời gọi đó khỏi translator. External **mất lưới an toàn**, trong khi payload
của nó **to hơn** (giữ đủ tool history có cấu trúc). Đây là đánh đổi cần chốt.

## Vì sao con số 900KB vốn đã sai cho external

`maxPayloadBytes` đo `payloadByteSize(payload)` — kích thước JSON của **`KiroPayload`**.
Wire body gửi tới gateway external là hình dạng **khác** (`kiroPayloadToOpenAIRequest` /
`kiroPayloadToResponsesRequest` / `kiroPayloadToAntigravityRequest`), nên 900KB của
KiroPayload không phải là ngân sách của thứ thực sự được gửi đi. Giới hạn thật của
upstream cũng khác nhau và phần lớn tính theo **token**, không theo byte.

Nên "external mất guard" nghe nghiêm trọng hơn thực tế: guard đó đang đo sai đại lượng.

## Lựa chọn

| | Cách | Đánh đổi |
|---|---|---|
| **(i)** | **Không guard.** Để upstream tự trả lỗi khi payload quá lớn. | Đơn giản nhất, đúng YAGNI. Request quá lớn fail với lỗi rõ từ upstream thay vì bị cắt im lặng. Rủi ro: fail muộn, tốn một lượt gọi. |
| (ii) | Giữ một guard chung, budget riêng cho external | An toàn hơn, nhưng phải chọn số — và không có dữ liệu nào để chọn đúng. Đoán sai thì hoặc vô dụng hoặc cắt nhầm. |
| (iii) | Guard theo dialect (chat / responses / anthropic) | Chính xác nhất, nhiều việc nhất, cần số liệu thật của từng gateway. |

## Đề xuất: (i)

Lý do:

1. **Không có bằng chứng nào cho thấy external đang gặp lỗi kích thước.** Log hiện có
   (`data/omniproxy.launchd.err.log`) ghi 276 `unclassified failure` — toàn bộ là lỗi
   transport và header timeout, không có ca nào là payload quá lớn.
2. Guard hiện tại đo **sai đại lượng** cho external (KiroPayload byte, không phải wire byte).
3. Cắt im lặng là hành vi xấu hơn fail rõ ràng: người vận hành không biết mình vừa mất
   history.
4. YAGNI — thêm một cơ chế không có dữ liệu để hiệu chỉnh là thêm một thứ phải bảo trì.

## Yêu cầu nếu chọn (i)

### Chức năng
1. External **không** bị cắt history. Payload đi nguyên.
2. Khi upstream trả lỗi kích thước, log **một dòng WARN** có account + model + byte size
   của wire body, để lần sau có số liệu mà quyết định (ii)/(iii).

### Phi chức năng
- Không thêm đường code mới nào chạy trong request path ngoài một phép đo khi **đã** lỗi.

## Thiết kế

Trong các adapter external, khi nhận lỗi 400/413 có dấu hiệu kích thước
(`context_length`, `too large`, `max_tokens`, `payload`), log:

```
[ExternalPayload] size-exceeded account=<label> model=<id> bytes=<n> dialect=<d>
```

Cạnh `externalWAFBlocked` (`external_openai.go:100`) theo cùng văn phong. Chỉ log, không
đổi luồng.

## File liên quan

**Sửa:**
- `proxy/external_openai.go`, `external_openai_responses.go`, `external_antigravity.go`,
  `external_codex.go` — thêm 1 lời gọi log ở site non-200
- `proxy/external_agentrouter.go` — như trên, cộng thêm tham số `payload` cho
  `callAgentRouterOpenAIRequest` (2 call site + chữ ký)

**Tạo:**
- `proxy/external_payload_size.go` — marker, nhận diện, log, resolve model
- `proxy/external_payload_size_test.go` — bảng nhận diện + 3 test end-to-end

## Thiết kế — đã làm, khác đề xuất

Đề xuất: helper cạnh `externalWAFBlocked` trong `external_openai.go`. Thực tế **tách file
mới** `proxy/external_payload_size.go` (~119 dòng) vì helper dùng chung cho **5 adapter**
và `external_openai.go` đã 2176 dòng.

**Nhận diện** (`externalPayloadSizeRejected`): điều kiện **413 trước, rồi 400 + marker**.
Ban đầu làm `status ∈ {400,413}` **và** marker, nhưng như vậy một `413` không kèm chữ nào
trong body bị bỏ — mà 413 tự nó đã nói request quá lớn, nên nó là **tín hiệu rõ nhất** mà
tính năng này tồn tại để thu. Giờ `413` trả `true` **không cần marker**; `400` (status phổ
biến nhất trên đường này, gần như luôn là lỗi field) vẫn phải khớp marker.

Cố ý **không** đưa `max_tokens` vào danh sách marker dù đề xuất có nêu: `"max_tokens must
be greater than 0"` là lỗi validation, không phải lỗi kích thước. Các trường hợp vượt
`max_tokens` thật vẫn bắt được qua `"exceeds the maximum"`.

**Model trong log** (`externalPayloadModel`): đi theo **đúng chuỗi của builder**
(`external_openai.go:346`) — `OriginalModel` → `ConversationState.CurrentMessage.
UserInputMessage.ModelID` → `"auto"`. Không dùng `PublicModel` (alias client thấy, không
bao giờ gửi lên) và không trả `"unknown"` khi payload rỗng, vì như vậy là ghi một model
gateway chưa từng thấy.

**5 site đã nối** (mỗi site chỉ gọi sau khi status != 200, rồi trả lỗi gốc **không đổi**):

| File | Dòng | dialect |
|---|---|---|
| `external_openai.go` | ~309 | `chat` |
| `external_openai_responses.go` | ~124 | `responses` |
| `external_antigravity.go` | ~1171 | `antigravity` |
| `external_codex.go` | ~384 | `codex` |
| `external_agentrouter.go` | ~133 | `agentrouter` |

**Thay đổi chữ ký:** `callAgentRouterOpenAIRequest` nhận thêm `payload *KiroPayload`. Hàm
này tự dựng wire body từ `baseBody` và **không có** payload, nên nếu không luồn tham số thì
log sẽ ghi `model=unknown`. Cả 2 call site đều nằm trong `callAgentRouterOpenAI` nơi payload
có sẵn. `CallAgentRouterTest` đi đường riêng, không đụng.

**Không nối** (đúng phạm vi — không mang conversation): `fetchExternalProviderModels`
(GET `/v1/models`, `external_openai.go:1501`), các đường đọc credit/usage/token refresh.

## Kiểm chứng — 6 phát hiện và cách sửa

Một vòng verify độc lập (3 finder + verify, chạy trước khi tắt ultracode) tấn công **danh
sách marker** bằng body lỗi thật của từng gateway. 6 phát hiện, tất cả đã sửa:

| # | Mức | Phát hiện | Sửa |
|---|---|---|---|
| 1 | HIGH | Anthropic: `input length and \`max_tokens\` exceed context limit: 205000 + 8192 > 200000` không khớp marker nào | thêm `context limit` |
| 2 | HIGH | Google: `Request payload size exceeds the limit: 20971520 bytes.` không khớp (`exceeds the limit` ≠ `exceeds the maximum`) | thêm `exceeds the limit` |
| 3 | MEDIUM | llama.cpp: `the request exceeds the available context size` không khớp | thêm `context size` |
| 4 | MEDIUM | `413` không kèm marker trong body thì **không** được log, dù comment gọi 413 là câu trả lời chuẩn của HTTP | `413` → `true` không cần marker |
| 5 | MEDIUM | Comment của `externalPayloadModel` nói `OriginalModel` là "ID thực sự gửi lên", **sai** khi account có `ModelMappings` (`applyExternalModelMapping` ghi đè sau đó) | viết lại comment: đây là ID **như được yêu cầu**, mapping là việc của từng adapter |
| 6 | LOW | Chuỗi fallback khác builder: dùng `PublicModel` (không bao giờ gửi lên), thiếu `ConversationState...ModelID` | theo đúng chuỗi builder, bỏ `PublicModel` |

Phát hiện 4 kéo theo một ca nữa khi probe lại: dạng **copula** (`Your request is too large`)
không khớp các marker dạng danh từ (`request too large`) — thêm marker `is too large`. Ở đây
false positive chỉ tốn một dòng WARN và không đổi luồng, còn miss thì mất đúng thứ tính năng
này tồn tại để thu, nên bất đối xứng nghiêng về phía thêm.

## Todo

- [x] Helper nhận diện lỗi kích thước từ body upstream
- [x] Log WARN kèm byte size thật của wire body
- [x] Test: body lỗi kích thước → log, **không** đổi luồng
- [x] Test: lỗi khác (401/402/403/429/500, 400 validation) **không** bị nhận nhầm
- [x] Test: 6 ca marker thật của Anthropic / Google / llama.cpp / copula
- [x] `go build ./... && go vet ./...`

## Tiêu chí hoàn thành — kết quả đo

| Tiêu chí | Đo được |
|---|---|
| Request vượt giới hạn fail với lỗi gốc, kèm một dòng log | `TestChatSizeRejectionIsLoggedAndFlowUnchanged`: lỗi trả về vẫn là `HTTP 400 … context_length_exceeded`, log đúng 1 dòng với `bytes=` khớp **chính xác** byte của wire body dựng lại |
| Không cắt im lặng cho external | không có đường code nào cắt; `bytes=` trong log chính là bằng chứng payload đi nguyên |
| `413` trần vẫn được log | `413 empty body` → `true` |
| Lỗi không phải kích thước không bị log | `400 validation`, `400 max_tokens must be > 0`, `401/403/429/500` → không có dòng nào |
| Dialect nào cũng log đúng model | `TestAgentRouterSizeRejectionIsLogged` (đường phải luồn thêm tham số `payload`) |

`go build ./...`, `go vet ./proxy/`, và toàn bộ suite đều sạch:
`go test ./... -count=1` → tất cả `ok` (proxy 15.9s). `TestSearchAdaptersUseNativeContracts`
bị skip vì sandbox chặn DNS — xem [[sandbox-dns-breaks-upstream-tests]].

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Gateway thật sự cần guard | Log cho số liệu; bật (ii) sau, khi đã biết ngưỡng thật |
| Nhận nhầm lỗi khác thành lỗi kích thước | `400` chỉ khớp khi body nêu tường minh một giới hạn; chỉ `413` mới đủ status một mình, và 413 không có nghĩa nào khác |

## Bảo mật

Log chỉ chứa số byte + account label + model — **không** nội dung message, **không** key.

## Bước tiếp theo

Nếu log cho thấy có ca thật, mở phase mới làm (ii). Nếu không, đóng ở đây.
