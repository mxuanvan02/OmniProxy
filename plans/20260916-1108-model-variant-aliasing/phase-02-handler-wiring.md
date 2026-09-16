# Phase 02 — Handler wiring + integration tests

**Ưu tiên:** Vừa · **Trạng thái:** Xong · **Phụ thuộc:** Phase 01

## Context Links

- [plan.md](plan.md) — quyết định D4
- [phase-01-alias-core.md](phase-01-alias-core.md) — `FindAvailableAliasModel`
- `proxy/external_openai.go:555` — `externalWireModelID` đọc `payload.OriginalModel`

## Overview

Helper `resolveModelAlias` probe một lần khi exact unavailable; wire vào 3 entry points
(Qwen messages, OpenAI chat, OpenAI Responses) NGAY SAU normalize/combo, TRƯỚC pool lookup.

## Key Insights

- **Bẫy đã gặp:** rewrite `req.Model` thôi chưa đủ. `originalModel` (nuôi
  `kiroPayload.OriginalModel` → `externalWireModelID`) được gán TRƯỚC điểm wire, nên nếu alias
  fire mà không cập nhật `originalModel`, gateway vẫn nhận tên cũ nó không phục vụ.
  Fix: khi alias fire thì gán cả `req.Model` lẫn `originalModel`.
- Combo/adaptive vẫn chạy trước alias: combo chứa tên thật, không cần alias.
- Pool giữ nguyên exact-match → zero regression cho luồng hiện tại.

## Requirements

- `resolveModelAlias(model)`: trả model nếu `HasAvailableAccountForModel`; nếu không hỏi
  `FindAvailableAliasModel`; log `[ModelAlias] old -> new` khi rewrite.
- Wire ở: `handleClaudeMessagesInternal` (sau `req.Model = routeModel`),
  `handleOpenAIChat` (sau `ParseModelAndThinking`), `handleOpenAIResponses` (tương tự).
- Usage log và outbound mang tên variant thật (minh bạch).

## Related Code Files

- Tạo: `proxy/model_alias.go` (24 dòng)
- Tạo: `proxy/model_alias_routing_test.go` (93 dòng)
- Sửa: `proxy/handler.go` (2 điểm wire + cập nhật originalModel)
- Sửa: `proxy/responses_handler.go` (1 điểm wire + cập nhật originalModel)

## Implementation Steps

1. Helper `resolveModelAlias` trong `proxy/model_alias.go`.
2. Wire 3 entry points; khi alias fire cập nhật cả `originalModel`.
3. Integration tests dùng `newSOTATestHandler` + upstream httptest capture wire model:
   - bare request → pool chỉ có `-cn` → 200 và upstream thấy `-cn`
   - exact available → upstream thấy đúng tên client gọi
   - pool chỉ có `-agent` → bare request phải fail, không nâng cấp ngầm

## Todo List

- [x] helper + 3 wire points + originalModel sync
- [x] integration tests xanh

## Success Criteria

```
$ go test ./proxy/ -run "TestOpenAIChat.*Variant|TestOpenAIChatKeepsExact" -count=1
--- PASS: TestOpenAIChatRoutesToDeploymentVariant
--- PASS: TestOpenAIChatKeepsExactModelWhenAvailable
--- PASS: TestOpenAIChatDoesNotSubstituteBehaviourVariant
```

Full `go test ./pool/` xanh; `go test ./proxy/` chỉ còn fail có sẵn
`TestSearchAdaptersUseNativeContracts` (sandbox chặn DNS, xem memory).

## Risk Assessment

- Double probe khi exact unavailable rồi alias cũng unavailable: trả nguyên model, lỗi gốc vẫn đến client.
- Race cooldown giữa probe và pool: pool lọc lại bằng bộ lọc của nó; worst case = hành vi hiện tại.

## Next Steps

Phase 03 — ghi chú vận hành.
