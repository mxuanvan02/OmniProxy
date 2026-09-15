# Điều tra: external account mất tool history

**Ngày:** 2026-09-15 · **Trạng thái:** đã chứng minh, chưa sửa
**Nguồn:** `proxy/external_history_tool_loss_test.go` (harness đo, không phải guard)

## Hiện tượng người dùng báo

Agent chạy qua OmniProxy tới provider external (apiforcode) có 2 triệu chứng:
1. **"Mất tool"** — model thuật lại việc sẽ gọi tool bằng văn xuôi
   (`Skill/tool: terminal — script Python đọc config.yaml...`) thay vì phát tool call thật.
2. **Lặp việc đã làm** — chạy lại y hệt một lệnh `ls` đã chạy trước đó.

## Cách đo

Dựng `ClaudeRequest` **qua JSON unmarshal** (đúng đường HTTP handler đi), gồm
3 vòng tool hoàn chỉnh + 1 active turn, rồi chạy `ClaudeToKiro` → `kiroPayloadToOpenAIRequest`
và đếm shape gửi lên upstream.

> **Bẫy đã sập một lần:** dựng `ClaudeRequest` trực tiếp bằng `[]ClaudeContentBlock` cho ra
> hội thoại **rỗng hoàn toàn** (0 assistant turn), vì `extractClaudeUserContent`
> (`proxy/translator.go:759`) chỉ nhận `[]interface{}` của `map[string]interface{}` —
> shape mà `json.Unmarshal` sinh ra. Test phải đi qua JSON, nếu không sẽ báo một bug
> nặng hơn nhiều so với bug thật.

## Kết quả

Client gửi 3 vòng tool hoàn chỉnh + 1 active turn. Upstream **nhận được**:

| # | role | tool_calls | content |
|---|---|---|---|
| 0 | system | 0 | "Tool execution constraint: …" |
| 1 | user | 0 | "start the task" |
| 2 | user | 0 | `Tool results:\n\n[run_command] OUTPUT_a` |
| 3 | user | 0 | `Tool results:\n\n[run_command] OUTPUT_b` |
| 4 | user | 0 | `Tool results:\n\n[run_command] OUTPUT_c` |
| 5 | assistant | **1** | "" ← chỉ còn active turn |
| 6 | tool | 0 | "OUTPUT_LAST" |
| 7 | user | 0 | "now do the next thing" |

**3 assistant turn mang tool call đã biến mất.** Chỉ active turn sống sót. Tool result
thành văn xuôi `user`, và vì assistant turn bị xoá nên có **3 lượt `user` liên tiếp**.

## Nguyên nhân

`sanitizeKiroHistory` (`proxy/translator.go:1636`) được gọi **vô điều kiện** từ cả
`ClaudeToKiro` (`:364`) và `OpenAIToKiro` (`:1380`). Nó tồn tại cho một ràng buộc của
**Kiro API**: "upstream chỉ nhận đúng một active tool turn". Comment trong hàm ghi rõ điều này.

Nhưng translator **không biết request sẽ route tới account nào** — sanitize chạy trước khi
pool chọn account. Nên external provider, vốn nhận được tool history có cấu trúc đầy đủ,
cũng bị áp ràng buộc của Kiro.

Hệ quả trực tiếp: model không nhìn thấy chính nó đã gọi tool gì ở các lượt trước. Nó chỉ
thấy tool output nằm trong lượt `user`. Với một agent nhiều bước, đây đúng là điều kiện
sinh ra cả hai triệu chứng — bắt chước văn xuôi thay vì gọi tool, và lặp lại việc đã làm.

Đây cũng là điều mà `proxy/tool_narration_pollution_test.go` được viết ra để chặn. Bản sửa
trước chỉ dọn **chữ** narration khỏi assistant turn; mất mát **cấu trúc** thì còn nguyên.

## Mức độ chắc chắn

- **Chắc chắn:** mất mát cấu trúc là thật, đo được, tái lập được.
- **Chưa chắc:** đây có phải nguyên nhân *duy nhất* của "mất tool". Test trực tiếp qua
  OmniProxy với 8 vòng tool vẫn cho ra tool call **đúng**, nên đây là suy giảm chứ không
  phải hỏng dứt khoát. Model TQ phía upstream cũng góp phần.

## Hướng sửa (chưa làm, cần quyết định kiến trúc)

Sanitize phải phụ thuộc **dialect đích**, không phải chạy mù. Hai đường:

1. **Đẩy sanitize ra khỏi translator** vào adapter Kiro (`CallKiroAPI`). Đúng bản chất:
   ràng buộc thuộc về Kiro, nên nó phải nằm ở tầng Kiro. Nhưng đụng đường đang chạy của
   65 account Kiro — rủi ro cao, cần golden test.
2. **Truyền cờ "đích là external"** vào `ClaudeToKiro`/`OpenAIToKiro`. Rẻ hơn, nhưng
   translator vẫn phải biết về routing, và chữ ký hàm đổi ở mọi call site.

Đường 1 đúng hơn về thiết kế; đường 2 rẻ hơn nhiều. **Cần người quyết trước khi làm.**

## Liên quan

- Chặn Phase 01 của plan này: adapter Anthropic cũng nhận payload đã bị sanitize, nên
  dialect mới sẽ thừa hưởng đúng mất mát này. Sửa sanitize **trước** thì adapter Anthropic
  mới có ý nghĩa.
- `proxy/tool_narration_pollution_test.go` — guard của bản sửa narration trước đó.
