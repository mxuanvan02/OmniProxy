# Kiro History Sanitize Relocation — Implementation Plan

**Goal:** Trả `sanitizeKiroHistory` về đúng tầng của nó — ràng buộc của **Kiro API** — để
external provider nhận được tool history có cấu trúc đầy đủ, mà **không đổi hành vi**
account Kiro.

**Bối cảnh:** Nối tiếp `plans/20260915-1636-external-dialect-standardization/`.
Mất mát đã đo: `.../reports/history-tool-loss.md`. Người dùng chốt hướng **A** (đẩy vào
tầng Kiro). Bản A **ngây thơ đã bị bác bỏ** bởi 3 lens độc lập: `reports/refactor-refutation.md`.

## Vì sao A ngây thơ hỏng

| # | Vấn đề | Bằng chứng |
|---|---|---|
| 1 | Sanitize và truncate **không giao hoán** — chỉ chuyển sanitize thì truncate đo history thô, fire nhầm | wireBytes 681→908, placeholder 0→1; cacheKey đổi |
| 2 | Bỏ sanitize làm lộ **tool result mồ côi** → `role:"tool"` không có `tool_calls` → **400 terminal** | `translator.go:333`, `:1852` |
| 3 | Payload **bị alias** giữa các account trong vòng xoay — fix thất bại theo thứ tự | assistant tool_calls 4→1 sau một attempt Kiro |

## Quyết định đã chốt

| # | Vấn đề | Chốt | Lý do |
|---|---|---|---|
| D1 | Điểm đặt transform | **`dispatchChat`** (`external_openai.go:1778`) | Là funnel **duy nhất**: native Kiro rơi xuống `:1811 return CallKiroAPI`. Account đã biết, gọi đúng 1 lần/attempt. |
| D2 | Sanitize + truncate | **Đi cùng nhau**, giữ thứ tự sanitize→truncate | Đo được: tách ra là hỏng cả hai đầu |
| D3 | Chống alias | **Trả bản copy** cho nhánh Kiro | `dispatchChat` nhận `payload` là tham số, gán lại **không** ảnh hưởng caller → con trỏ gốc còn nguyên cho attempt sau |
| D4 | Orphan tool result | **Chuẩn hoá ở tầng translator**, áp cho **cả hai** đường | Là lỗi cấu trúc, không phải ràng buộc Kiro |
| D5 | Chính sách kích thước cho external | **Chưa chốt** — xem phase 03 | Hôm nay external được cắt (trên bản đã sanitize nên hiếm khi fire); sau refactor là không còn gì |

**Ngoài phạm vi:** bug có sẵn `truncatePayloadToLimit` có thể vượt `maxPayloadBytes`
(`translator.go:1816`) — ghi nhận, không sửa trong plan này.

## Task Index

| Phase | Nội dung | Rủi ro | Trạng thái |
|---|---|---|---|
| [01](phase-01-orphan-tool-result-normalization.md) | Chuẩn hoá orphan tool result (cả 2 đường) | Vừa | **Đã làm** — code + test xanh, nhưng INERT hôm nay (đo: 0 orphan tới payload). Gộp vào commit của 02 |
| [02](phase-02-relocate-sanitize-and-truncate.md) | Chuyển sanitize+truncate vào `dispatchChat`, trả bản copy | **Cao** | **Đã làm** — byte-identical, chưa commit |
| [03](phase-03-external-payload-size-policy.md) | Chính sách kích thước payload cho external | Thấp | Chưa làm — chốt option (i): không guard, chỉ WARN |

**Thứ tự bắt buộc:** 01 trước 02. Phase 01 một mình đã là fix đúng và test được độc lập;
phase 02 mà không có 01 là gây 400. Phase 03 chạy sau 02.

## Chặn

Plan này **chặn Phase 01 của `20260915-1636-external-dialect-standardization`** (adapter
Anthropic External). Adapter Anthropic nhận payload qua `dispatchChat`, nên nếu chưa sửa
sanitize thì dialect mới thừa hưởng đúng mất mát cấu trúc đã đo.

## Bằng chứng đo được (2026-09-15)

Thí nghiệm trên bản copy `$TMPDIR`, không sửa cây repo. Fixture 3 vòng tool:

| | assistant có tool_calls | role:tool |
|---|---|---|
| Wire external hôm nay | 1 | 1 |
| Sau khi bỏ sanitize khỏi translator | **4** | **4** |

Các adapter **đã có sẵn nhánh xử lý** shape có cấu trúc
(`external_openai.go:405-413/453-459`, `responses_upstream.go:99-119/131-141`,
`external_antigravity.go:747-767/781-795`) — chúng là **code chết** vì sanitize đã nil mất
`ToolUses`/`ToolResults`. Đây là cơ sở để tin fix sẽ chạy đúng ở đường phổ biến.

## Global Constraints

- Không thêm dependency Go mới.
- `go build ./...` và `go vet ./...` phải sạch sau mỗi phase.
- Hành vi account Kiro phải **byte-identical** dưới ngưỡng truncate; trên ngưỡng phải
  chứng minh bằng test, không suy luận.
- File code mới dưới 200 dòng, một trách nhiệm rõ ràng.
- Comment giải thích "tại sao", không "cái gì" — theo văn phong repo.
- `go test ./proxy/` cần `sandbox.network.allowLocalBinding`, nếu không panic khi bind port.
- Commit conventional, không tham chiếu AI.
- Soi `git diff --cached` trước mỗi commit.
- **Repo đang có session khác chạy song song** (plan `20260915-1622-providers-view`) —
  không đụng file ngoài phạm vi plan này.
