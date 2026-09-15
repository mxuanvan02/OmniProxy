# Phase 01 — Chuẩn hoá orphan tool result

**Ưu tiên:** Vừa · **Trạng thái:** Đã làm — chưa commit · **Phụ thuộc:** không
**Chặn:** phase 02

## Bối cảnh

Hôm nay `sanitizeKiroHistory` **che** một lỗi cấu trúc: nó narrate history tool result
thành văn xuôi *trước khi* `truncatePayloadToLimit` chạy, nên không ai thấy rằng hai hàm
cắt history có thể để lại `tool_result` mồ côi.

Bỏ sanitize khỏi translator là lỗi đó lộ ra ngay. Phase này sửa nó **trước**, độc lập,
có test riêng.

## Trạng thái: ĐÃ LÀM (code + test xanh), nhưng INERT hôm nay

Đo bằng probe trên chính `ClaudeToKiro` (fixture assistant-first ở dưới):

```
PROBE: history tool results reaching payload = 0
```

`sanitizeKiroHistory` chạy ở `translator.go:364/366`, **trước** `truncatePayloadToLimit`
(`:419`), và nó đã nil toàn bộ `ToolResults` trong history. Nên
`stripOrphanedToolResults` chạy cuối translator **không tìm thấy gì để bỏ**.

Hệ quả cần biết khi chốt commit:

- Code hôm nay là **no-op trên mọi request thật**. Test của nó phủ **hàm**, không phủ
  **đường chạy** nào.
- `TestClaudeToKiroLeavesNoOrphanedToolResults` đang **xanh rỗng** — cùng loại với
  `TestClaudeToKiroKeepsActiveToolTurnStructured` ở phase 02. Nó chỉ có nghĩa sau khi
  phase 02 bỏ narration.
- Guard trở nên **load-bearing đúng lúc phase 02 bỏ sanitize** — đó là lý do phase 01
  được khai báo chặn phase 02.

## Bằng chứng

Đo trên bản copy `$TMPDIR`, fixture assistant-first
(`TestClaudeToKiroDropsLeadingAssistantHistory`, `translator_test.go:407`):

```
Hôm nay:   [system, user, user]
Sau move:  [system, user(placeholder), tool(t2), assistant_toolcall(t3), tool(t3), user]
                                      ↑ role:tool(t2) không có tool_calls trước nó
```

Cùng hình dạng ra cả 3 dialect: `role:"tool"` (chat), `function_call_output` không có
`function_call` (Responses), `functionResponse` — Gemini đòi tên phải khớp call.

**400 là terminal** (`account_failover.go:77-78`): request fail hẳn, không xoay account.

## Hai nguồn sinh orphan

| Nguồn | File | Hành vi |
|---|---|---|
| `trimLeadingAssistantHistory` | `translator.go:333` (gọi), `:1935` (định nghĩa) | Bỏ assistant turn đầu history |
| `dropLeadingAssistant` | `translator.go:1852` | Bỏ assistant turn đầu của tail sau truncate |

Cả hai chỉ bỏ **nửa đầu** của cặp call/result.

## Yêu cầu

### Chức năng
1. Bỏ một assistant turn có `ToolUses` thì phải bỏ luôn user turn ngay sau nó **nếu**
   toàn bộ `ToolResults` của turn đó được trả lời bởi chính assistant turn vừa bỏ.
2. Lặp, vì bỏ turn sau có thể lộ ra một assistant turn mới ở đầu.
3. Không được bỏ user turn mang text thật — chỉ bỏ khi `ToolResults` là toàn bộ nội dung
   có nghĩa của nó.
4. Áp cho **cả hai** translator (Claude và OpenAI) và cho **cả hai** nguồn trên.

### Phi chức năng
- Không đổi hành vi khi history đã hợp lệ (mọi test hiện có phải xanh).
- Sau phase này, **bất biến** phải giữ: mọi `ToolResults` trong history đều có assistant
  turn cung cấp `toolUseId` tương ứng ở **trước** nó.

## Thiết kế

Gom hai nguồn về **một** helper cạnh `dropLeadingAssistant`:

```go
// dropOrphanedToolResults removes the leading assistant turn together with the
// user turn that answers it. Dropping only the assistant half leaves a
// tool_result with no preceding tool_use, which every external upstream rejects
// with a terminal 400 (the OpenAI, Responses and Gemini dialects alike). Loops,
// because removing the pair can expose another assistant turn at the head.
func dropOrphanedToolResults(tail []KiroHistoryMessage) []KiroHistoryMessage
```

`dropLeadingAssistant` trở thành lời gọi helper này (hoặc bị thay hẳn), và
`trimLeadingAssistantHistory` cũng vậy.

**Thứ tự quan trọng:** helper phải chạy ở nơi **sau cùng** cắt history — tức trong
`truncatePayloadToLimit` sau `dropLeadingAssistant` — **và** ở `trimLeadingAssistantHistory`
trong translator. Hai chỗ, cùng một hàm.

### Vì sao bỏ hẳn cặp, không fold thành text

Fold thành text là cách `buildCurrentMessageContent` xử lý orphan ở **current message** —
ở đó giữ thông tin là đúng vì model còn dùng được. Ở history thì khác: lượt assistant
chứa call đã bị cắt khỏi context, nên kết quả đó là **quá khứ không còn tham chiếu**.
Giữ nó dưới dạng text chỉ thêm nhiễu. Bỏ cả cặp là khớp với ngữ nghĩa cắt history.

## File liên quan

**Sửa:**
- `proxy/translator.go` — `dropLeadingAssistant:1852`, `trimLeadingAssistantHistory:1935`,
  chỗ gọi trong `truncatePayloadToLimit` (`:1822`)

**Tạo:**
- `proxy/history_orphan_test.go`

## Todo

- [x] `stripOrphanedToolResults` — strip kết quả mồ côi, giữ nguyên text/image của turn
- [x] Áp trong `ClaudeToKiro` và `OpenAIToKiro`, **sau** `truncatePayloadToLimit`
- [x] Test: assistant-first → không còn result mồ côi
- [x] Test: user turn mang **text thật** → giữ text, chỉ bỏ result
- [x] Test: call xuất hiện **sau** result không tính là ghép cặp
- [x] Test: slice kết quả được dựng lại, không ghi đè backing array của caller
- [x] Test hồi quy: fixture không orphan cho body **byte-identical**
- [x] `go build ./... && go vet ./proxy/`
- [x] `go test ./proxy/` — xanh (trừ `TestSearchAdaptersUseNativeContracts/jina-reader`,
      **pre-existing**: DNS `example.com` bị sandbox chặn, fail y hệt trên cây sạch)

## Thiết kế thực tế đã làm (khác đề xuất ban đầu)

Đề xuất ban đầu là `dropOrphanedToolResults` bỏ **cả cặp** assistant+user, gọi từ
`dropLeadingAssistant` và `trimLeadingAssistantHistory`. Bản đã làm khác:

`stripOrphanedToolResults(history)` chạy **một lần, cuối cùng**, trong cả hai translator.
Lý do:

1. Bỏ cả cặp là sai khi user turn mang **text thật** — chỉ bỏ `ToolResults`, giữ text.
   Làm ở tầng "bỏ turn" thì không tách được hai thứ đó.
2. Đặt ở cuối phủ **mọi** nguồn sinh orphan (hiện tại 2, sau này có thể thêm) bằng một
   điểm, thay vì phải nhớ gọi ở từng chỗ cắt history.
3. Bất biến được kiểm trên **hình dạng cuối cùng** của payload, đúng thứ tự đến upstream.

Hàm chạy quét tiến: một result chỉ được coi là ghép cặp khi call của nó xuất hiện
**nghiêm ngặt trước** nó.

## Tiêu chí hoàn thành

- Fixture assistant-first: adapter chat không còn emit `role:"tool"` không có `tool_calls`;
  adapter Responses không còn `function_call_output` mồ côi.
- Mọi test translator hiện có xanh, không sửa kỳ vọng.
- Bất biến "không orphan" được assert trong test mới.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Bỏ quá tay user turn mang nội dung thật | Chỉ bỏ khi `ToolResults` phủ toàn bộ và không có Images; test riêng cho ca này |
| Đổi hành vi Kiro ngoài ý muốn | Đo byte-identical trên fixture không orphan |

## Bảo mật

Không đụng credential. Không log nội dung message.

## Bước tiếp theo

Phase 02 dựa trên bất biến này. Không làm 02 trước 01.
