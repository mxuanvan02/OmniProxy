# Model Variant Aliasing — Implementation Plan

**Goal:** Agent chỉ cần gọi tên canonical (`qwen3.8-max`); OmniProxy tự resolve sang
variant triển khai thực sự available (`qwen3.8-max-cn`, snapshot `-0813`, lệch case/dash)
khi tên exact không có account nào khỏe phục vụ. Outbound mang tên variant thật.

**Bối cảnh:** Provider như `apiforcode.com` expose nhiều variant triển khai của một model.
Routing trước đây exact-match (`pool/account.go accountHasModel`), nên `qwen3.8-max`
không route được vào account chỉ có `qwen3.8-max-cn`.

## Quyết định đã chốt

| # | Vấn đề | Chốt | Lý do |
|---|---|---|---|
| D1 | Variant nào được gộp alias | Chỉ variant triển khai: locale `-cn`/`-on`, snapshot `-\d{4}`, case, dash | Variant hành vi (`-agent`, `-thinking-agent`, `-flash`, effort) là model khác; agent tự gọi |
| D2 | Shape `/v1/models` | Giữ nguyên raw | Alias hoạt động ngầm ở routing; không phá client hardcode tên variant |
| D3 | Thứ tự ưu tiên variant | exact > bare > locale theo thứ tự config > snapshot mới nhất; mọi candidate qua bộ lọc sức khỏe | Không thêm config mới (YAGNI); sức khỏe vẫn là cửa cuối |
| D4 | Vị trí rewrite | Handler entry (Qwen/OpenAI chat/Responses), trước pool lookup | Outbound `OriginalModel` và usage log mang tên thật; pool giữ exact-match, zero regression |

## Task Index

| Phase | Nội dung | Rủi ro | Trạng thái |
|---|---|---|---|
| [01](phase-01-alias-core.md) | `canonicalModelKey` + `aliasCandidates` + `FindAvailableAliasModel` + unit tests | Vừa | Xong |
| [02](phase-02-handler-wiring.md) | `resolveModelAlias` helper + wire 3 entry points + integration tests | Vừa | Xong |
| [03](phase-03-operations-notes.md) | Ghi chú vận hành: đọc log `[ModelAlias]`, escape hatch | Thấp | Xong |

## Thứ tự bắt buộc

Phase 02 phụ thuộc 01. Phase 03 là docs, chạy cuối.

## Global Constraints

- Không thêm dependency Go mới.
- `go build ./...` và `go vet ./proxy/ ./pool/` sạch sau mỗi phase.
- Không đổi hành vi exact-match hiện có: alias chỉ chạy khi exact unavailable.
- File code mới dưới 200 dòng.
- Comment giải thích "tại sao".
- Commit conventional, không tham chiếu AI; soi `git diff --cached` trước commit.
