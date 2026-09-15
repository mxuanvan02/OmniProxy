# Refactor bị bác bỏ: bằng chứng phản biện

**Ngày:** 2026-09-15 · 3 lens độc lập, đều **REFUTED**, confidence **high**
**Phương pháp:** mỗi lens copy module vào `$TMPDIR`, xoá 4 call site
`sanitizeKiroHistory` (`translator.go:364/366/1380/1382`), chạy lại pipeline thật
(`ClaudeToKiro` → adapter), so với bản gốc. **Cây repo không bị sửa.**

## Phát hiện lớn nhất: sanitize và truncate không giao hoán

Thứ tự hiện tại (`translator.go`):

```
:358  currentToolResultIDs    ← history thô
:364  sanitizeKiroHistory     ← flatten
:419  truncatePayloadToLimit  ← đo trên history ĐÃ flatten
```

Sanitize xoá `ToolUses[].Input` JSON và biến tool result thành văn xuôi. Trong fixture
đo được, history co từ ~1.5MB xuống ~700B — **nhỏ đi ~1000 lần**.

Nếu chỉ chuyển sanitize mà để truncate lại, truncate đo history thô → **fire ở chỗ
trước đây không fire**. Đo được:

| Fixture | Hôm nay | Nếu chỉ chuyển sanitize |
|---|---|---|
| 5× tool_use ~300KB | wireBytes=681, history=4, placeholder=0 | 908, 5, **1** |
| 4× 300KB + active turn | 944, 5, 0 | 1171, 6, **1** |
| retry loop 100× 10KB | 10,680, 4, 0 | 10,803, 4, **1** |
| không system prompt | cacheKey `0f6e0c…` | cacheKey `6519c6…` |

Dòng cuối nguy hiểm nhất: truncate fire → `history[0]` thành placeholder → không còn
bắt đầu bằng `"You are "` → `payloadCodexInstructions` (`handler.go:4696`) trả `""` →
`payloadCacheKey` rơi về `payloadConversationPrefix` → **pin sang account khác**.

Đảo ngược lại cũng hỏng: 30 vòng tool, hôm nay giữ 19 lượt narrated, nếu đảo thứ tự
chỉ còn 10 — tức là **tự phá chính mục tiêu của fix**.

## Phát hiện nguy hiểm thứ hai: tool result mồ côi → 400 terminal

Hai chỗ sinh ra orphan:

- `trimLeadingAssistantHistory` (`translator.go:333`) — bỏ assistant turn đầu, để lại
  `tool_result` của nó.
- `dropLeadingAssistant` (`translator.go:1852`) — y hệt, trong nhánh truncate.

Hôm nay sanitize **che** cả hai: nó narrate result thành text *trước khi* truncate chạy.
Bỏ sanitize khỏi translator là hai chỗ này lộ ra:

```
Hôm nay:  [system, user, user]                        ← không orphan
Sau move: [system, user(placeholder), tool(t2), assistant_toolcall(t3), tool(t3), user]
                                     ↑ role:tool không có tool_calls trước nó
```

Cùng hình dạng đó ra tới cả 3 dialect: `role:"tool"` (chat), `function_call_output`
không có `function_call` (Responses), `functionResponse` (Gemini — tên phải khớp call).

**400 là terminal** (`account_failover.go:77-78`) → request **fail hẳn**, không xoay
account. Đây là thứ biến "sửa một suy giảm" thành "gây sự cố".

Không cần payload to: `TestClaudeToKiroDropsLeadingAssistantHistory`
(`translator_test.go:407`) là đúng hình dạng này.

## Phát hiện thứ ba: payload bị alias giữa các account

`dispatchChat` chạy trong vòng xoay account, **dùng lại một con trỏ `*KiroPayload`**
(`handler.go:3846/4847/5206/5694`, `responses_handler.go:184/468`). Pool là **một pool
hỗn hợp** — `pool/account.go:691` Phase 2 xoay qua mọi account, kể cả native Kiro. Nên
trong **cùng một request**, một attempt Kiro có thể chạy trước một attempt external.

Sanitize đặt trong `CallKiroAPI` sửa payload tại chỗ và không hoàn tác → attempt external
sau đó nhận history đã bị Kiro flatten:

| | assistant có tool_calls | role:tool |
|---|---|---|
| không có attempt Kiro trước | 4 | 4 |
| sau 1 lần sanitize | 1 | 1 |

Fix **im lặng thất bại theo thứ tự**, không test nào bắt được.

## Đã kiểm tra và KHÔNG phải hazard

- `sanitizeKiroHistory` **idempotent** — gọi 2 lần cho body byte-identical trên cả 6
  fixture, kể cả fixture over-budget.
- `currentToolResultIDs` **khôi phục được** từ payload: translator gắn `currentToolResults`
  nguyên vẹn khi `keepCurrentToolResults`, nil khi không (`translator.go:392-399`), nên
  `collectToolResultIDs(CurrentMessage...ToolResults)` tái lập chính xác **cả hai nhánh**.
- `toolNames` khôi phục được từ history **thô**. Cảnh báo: phải build **trước** khi
  truncate, nếu không mất tên tool — đo được `[run_command] OUTPUT_0` → `OUTPUT_0`, đúng
  loại mất mát mà comment `translator.go:1690-1697` cấm.
- `payloadCacheKey` / `payloadConversationPrefix` chỉ đọc `history[0]/[1]` + `ConversationID`
  → SAFE.
- Token estimation đọc **client request**, không đọc payload → SAFE.
- Context usage lấy từ upstream → SAFE.
- `applyExternalCacheControl` index breakpoint 2 có thể trỏ vào `role:"tool"` — **có sẵn
  từ trước**, không phải do refactor.
- Không có con trỏ payload nào bị cache/goroutine giữ; alias duy nhất là vòng xoay.

## Bug có sẵn, phát hiện thêm (ngoài phạm vi)

`truncatePayloadToLimit` có thể để payload **vượt** `maxPayloadBytes`: guard
`kept > minRecentHistoryTurns` (`translator.go:1816`) từ chối break, nên một entry
950KB sống sót → body đo được 974,511 bytes vs giới hạn 921,600.

## Hệ quả cho thiết kế

Ba phát hiện trên định hình luôn thiết kế đúng:

1. Sanitize và truncate **phải đi cùng nhau**, giữ nguyên thứ tự sanitize→truncate.
2. Phải có bước **chuẩn hoá orphan tool result** cho **cả hai** đường — không thì refactor
   tự gây 400.
3. Transform phải trả về **bản copy**, để con trỏ gốc của caller còn nguyên.
