# Phase 02 — Chuyển sanitize + truncate vào tầng Kiro

**Ưu tiên:** Cao · **Trạng thái:** Đã làm — chưa commit · **Phụ thuộc:** phase 01
**Rủi ro:** Cao — đụng đường chạy của mọi account Kiro

## Bối cảnh

`sanitizeKiroHistory` là ràng buộc của **Kiro API** ("chỉ một active tool turn"), nhưng
đang chạy trong translator — nơi **chưa biết** request sẽ route tới account nào. Hệ quả:
external provider cũng bị áp ràng buộc Kiro. Mất mát đã đo ở
`plans/20260915-1636-.../reports/history-tool-loss.md`.

Bản A ngây thơ bị bác bỏ — xem `reports/refactor-refutation.md`. Phase này làm bản đúng.

## Key insights

1. **`dispatchChat` (`external_openai.go:1778`) là funnel duy nhất.** Native Kiro rơi
   xuống `:1811 return CallKiroAPI`. External đi các nhánh trên. Nên đặt transform ở đây
   là phủ **cả hai** đường bằng một điểm, với `account` đã biết, gọi đúng **một lần mỗi attempt**.
2. **Sanitize và truncate không giao hoán.** Đo được: tách ra thì truncate đo history thô
   → fire nhầm (wireBytes 681→908, placeholder 0→1) và **đổi `payloadCacheKey`** → pin sai
   account. Phải đi cùng nhau, giữ thứ tự sanitize→truncate.
3. **Trả bản copy là đủ để chống alias.** `payload` là **tham số** của `dispatchChat`;
   gán lại trong hàm **không** ảnh hưởng biến của caller. Nên attempt external sau đó
   trong cùng vòng xoay vẫn thấy history thô.
4. **`hasPriming` suy ra được.** `truncatePayloadToLimit` cần `systemPrompt != ""`
   (`translator.go:373/1426`) — không lưu trên payload. Nhưng `payloadCodexInstructions`
   (`handler.go:4696`) đã có sẵn cách nhận diện cặp priming; dùng lại logic đó.
5. **`toolNames` phải build TRƯỚC truncate.** Đo được: nếu `dropLeadingAssistant` bỏ
   assistant turn trước khi sanitize build map, tên tool biến mất —
   `[run_command] OUTPUT_0` → `OUTPUT_0`. Thứ tự sanitize→truncate giữ đúng điều này.

## Yêu cầu

### Chức năng
1. `ClaudeToKiro` và `OpenAIToKiro` **thôi** gọi `sanitizeKiroHistory` (4 chỗ:
   `translator.go:364/366/1380/1382`) và `truncatePayloadToLimit` (2 chỗ: `:419/:1426`).
2. `dispatchChat` chuẩn bị payload theo **đích**: nhánh Kiro nhận bản đã sanitize+truncate;
   nhánh external nhận **nguyên bản thô**.
3. Bản chuẩn bị là **copy** — payload của caller không bị sửa.
4. `currentToolResultIDs` tính lại từ
   `payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults`.

### Phi chức năng
- Account external: wire body phải chứa **đủ** tool history có cấu trúc.
- Account Kiro: dưới ngưỡng truncate, wire body **byte-identical** với hôm nay.
- Trên ngưỡng truncate: hành vi phải **có test chứng minh**, không suy luận.

## Thiết kế thực tế đã làm

Đề xuất ban đầu (giữ ở mục dưới để đối chiếu) giữ nguyên hình dạng chính:
`prepareKiroPayload` copy nông + clone history, sanitize rồi truncate, gọi tại `dispatchChat`.
Các điểm **lệch so với đề xuất**, đều có lý do đo được:

1. **`payloadHasPriming` bị bỏ.** Không suy ra được từ payload: history do client gửi có thể
   mở đầu bằng đúng cặp user+assistant giống cặp priming, nên đoán sẽ sai. Thay bằng field
   `hasPriming` trên `KiroPayload`, translator set (`payload.hasPriming = systemPrompt != ""`).
   `cloneKiroPayload` (`content_block_retry.go`) copy luôn field này.
2. **`prepareKiroPayload` không nhận tham số priming.** Đọc từ `cp.hasPriming` — chữ ký
   `func prepareKiroPayload(payload *KiroPayload) *KiroPayload`.
3. **`stripOrphanedToolResults` (phase 01) Ở LẠI translator**, không dời vào đây. External
   cũng cần nó — external không đi qua `prepareKiroPayload` — nên dời đi là gây orphan cho
   chính nhánh vừa được cứu.
4. **Gộp `collectToolResultIDs` + `currentToolResultsMatchLastAssistant`** vào
   `currentToolResultsOf`; logic quyết định giữ nguyên như code cũ.
5. **Test harness:** `kiro_payload_prepare_test.go` (đơn vị) +
   `kiro_wire_baseline_test.go` + `proxy/testdata/kiro_wire_baseline.json` (hồi quy byte).
   Không tách `dispatch_kiro_prepare_test.go` riêng.
6. **`currentToolResultsMatchLastAssistant` giữ nguyên**, vẫn là điều kiện quyết định nhánh
   nào của `sanitizeKiroHistory` chạy; chỉ phần thu thập ID được gom vào
   `currentToolResultsOf`.

### Đề xuất ban đầu (đối chiếu)

```go
cp := *payload                                          // ConversationState là value
cp.ConversationState.History = cloneHistoryForSanitize(payload.ConversationState.History)
ids := collectToolResultIDs(currentToolResultsOf(&cp))
if currentToolResultsMatchLastAssistant(cp.ConversationState.History, ids) {
    cp.ConversationState.History = sanitizeKiroHistory(cp.ConversationState.History, ids)
} else {
    cp.ConversationState.History = sanitizeKiroHistory(cp.ConversationState.History, nil)
}
truncatePayloadToLimit(&cp, payloadHasPriming(&cp))
return &cp
```

Copy nông `cp := *payload` là đủ vì `CurrentMessage.UserInputMessage` là **field value**;
`truncateCurrentMessage` (`translator.go:1875`) chỉ sửa `Content`. **Đã verify**: nó không
sửa `UserInputMessageContext` (pointer dùng chung) — đây là lý do copy nông an toàn.

### Điểm chèn

`dispatchChat`, trước nhánh Kiro:
```go
return CallKiroAPI(ctx, account, prepareKiroPayload(payload), callback)
```
Bản copy giải quyết alias mà không phải đụng 6 chỗ vòng xoay.

## File liên quan

**Sửa:**
- `proxy/translator.go` — bỏ 4 lời gọi sanitize và 2 lời gọi truncate; set `hasPriming`
- `proxy/kiro.go` — thêm field `hasPriming` vào `KiroPayload`
- `proxy/content_block_retry.go` — `cloneKiroPayload` copy `hasPriming`
- `proxy/external_openai.go` — nhánh Kiro trong `dispatchChat` dùng `prepareKiroPayload`
- 9 chỗ trong test cũ bọc lại bằng `prepareKiroPayload(...)` (nếu không chúng xanh rỗng)

**Tạo:**
- `proxy/kiro_payload_prepare.go` — `prepareKiroPayload` + `cloneHistoryForSanitize`
- `proxy/kiro_payload_prepare_test.go` — test đơn vị
- `proxy/kiro_wire_baseline_test.go` + `proxy/testdata/kiro_wire_baseline.json` — hồi quy byte

## Todo

- [x] `cloneHistoryForSanitize` + test: sửa bản copy **không** đụng bản gốc
- [x] Priming: field `hasPriming` trên IR thay vì `payloadHasPriming` suy luận
- [x] `prepareKiroPayload` — sanitize rồi truncate, đúng thứ tự
- [x] Bỏ 4 + 2 lời gọi khỏi translator
- [x] Nhánh Kiro trong `dispatchChat` dùng bản copy
- [x] Verify `truncateCurrentMessage` không sửa `UserInputMessageContext`
- [x] Hồi quy byte-identical: 12 fixture trong `testdata/kiro_wire_baseline.json`
- [x] Test trên ngưỡng: Kiro body khớp body trước refactor
- [x] Test: external body có **đủ** assistant tool_calls + role:tool (1→4 với 3 vòng)
- [x] Test: attempt Kiro rồi attempt external cùng payload → external vẫn thô
- [x] `go build ./... && go vet ./... && go test ./proxy/`

## Tiêu chí hoàn thành — kết quả đo

| Tiêu chí | Kết quả |
|---|---|
| 3 vòng tool qua external: `assistant tool_calls` 1→4, `role:tool` 1→4 | **4 / 4** — đo trong `TestExternalProviderReceivesFullToolHistory` |
| Kiro dưới ngưỡng: byte-identical | **10/12 fixture byte-identical** với bytes trước refactor |
| Kiro trên ngưỡng: byte-identical | **đạt** — cả 3 fixture truncating đều khớp |
| Fixture assistant-first: không orphan | **đạt** — 2 fixture đổi đúng chỗ dự kiến, có `note` |
| Sau một attempt Kiro, payload gốc **không đổi** | **đạt** — `TestPrepareKiroPayloadLeavesInputUntouched` so con trỏ + nội dung |

**2 fixture đổi** là chủ ý của phase 01: `u(content=20)` (narration của tool result cũ) →
`u(content=1)` (placeholder fallback), tức orphan đã bị bắt trước khi tới sanitize.

**Test có giá trị thật, đã chứng minh:** probe chạy trên cây pristine tại HEAD (sanitize còn
trong translator) đo được external **1/4** — đúng lỗi. Nên `TestExternalProviderReceivesFullToolHistory`
thực sự fail trên code cũ, không phải test trang trí.

## Kiểm thử — tình trạng hiện tại phải biết

Mutation test (xoá 4 call site) cho thấy **8 test fail**, và:

- **Không có test nào** phủ sanitize ở `CallKiroAPI`/`dispatchChat`.
- `TestClaudeToKiroKeepsActiveToolTurnStructured` (`translator_compaction_test.go:100`)
  trở thành **xanh rỗng** — không còn gì strip active turn nên nó pass vô nghĩa. Phải
  chuyển sang harness mới ở tầng dispatch.
- `TestDiagnoseExternalHistoryToolLoss` (`external_history_tool_loss_test.go:18`) chỉ
  `t.Logf`, **không assert gì** — không được tính là coverage.
- `stripPollutedToolCallText` (`:1564`) và `narrateToolResults` (`:1585`) mất đường test
  duy nhất qua translator.
- `translator_truncate_test.go:13` **không bắt được** lỗi đảo thứ tự vì fixture không có
  tool cycle.

Việc của phase này gồm **dựng harness mới** build `KiroHistoryMessage` trực tiếp và gọi
qua `dispatchChat`, nếu không coverage về 0 ở nhà mới.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Đảo thứ tự sanitize/truncate | Giữ nguyên thứ tự trong `prepareKiroPayload`; test trên ngưỡng so byte với bản gốc |
| Quên một call site → Kiro nhận history thô → 400 | grep `sanitizeKiroHistory` phải còn **đúng 1** lời gọi, trong `kiro_payload_prepare.go` |
| Copy nông để lọt mutation | Test assert payload gốc không đổi sau `prepareKiroPayload` |
| `payloadCacheKey` đổi → pin sai account | Test cacheKey trước/sau trên cùng fixture |
| Evasion path đổi thứ tự | `applyLearnedObliteration` chạy ở `dispatchChat:1786`, **trước** nhánh Kiro — giữ nguyên vị trí; AgentRouter không đi nhánh Kiro nên không đổi |

## Bảo mật

Không đụng credential. `prepareKiroPayload` không log nội dung history.

## Bước tiếp theo

Phase 03 chốt chính sách kích thước cho external. Phase 01 của plan
`20260915-1636-external-dialect-standardization` (adapter Anthropic) **chờ phase này xong**.
