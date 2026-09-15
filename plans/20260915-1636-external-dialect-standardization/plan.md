# External Dialect Standardization — Implementation Plan

**Goal:** External account chọn được 3 dialect (`chat` | `responses` | `anthropic`) ngay khi add key,
`responses` là mặc định; cả 3 bám đúng spec chính thức; usage log hiện dialect để minh bạch.

**Bối cảnh:** Nối tiếp `plans/20260915-0047-external-responses-dialect/` (Phase 01–03 đã merge:
`ExternalAPIDialect`, `externalAPIDialect()`, `CallExternalOpenAIResponses`, UI dropdown).
Plan này mở rộng, không viết lại.

## Quyết định đã chốt (không hỏi lại)

| # | Vấn đề | Chốt | Lý do |
|---|---|---|---|
| D1 | 65 account cũ đang `externalApiDialect` rỗng | **Giữ `chat`** | Rỗng = chat như hiện tại. Đổi ngầm sang responses sẽ đánh sập account nào gateway chỉ có chat, không có tín hiệu nào báo trước. |
| D2 | Form add key mới | **Default `responses`** | Đúng yêu cầu: responses mới và tốt hơn. |
| D3 | Gateway trả 404/405 ở `/v1/responses` | **Fallback sang chat + ghi nhớ** | Chỉ trả giá một lần cho mỗi account; self-healing. |
| D4 | Anthropic External | **Dialect thứ 3, chung account external** | baseUrl + accessToken là cùng một thứ; chỉ wire dialect khác. |
| D5 | Anthropic gửi key kiểu gì | **Cả `x-api-key` lẫn `Authorization: Bearer`** | Gateway resale không thống nhất; thừa một header vô hại, thiếu thì 401. |

**Ngoài phạm vi:** sửa dialect sau khi add (giữ giới hạn của plan trước — xoá rồi add lại).

## Task Index

| Phase | Nội dung | Rủi ro | Trạng thái |
|---|---|---|---|
| [01](phase-01-anthropic-external-adapter.md) | Adapter `CallExternalAnthropic` + dialect `anthropic` | **Cao** | Xong |
| [02](phase-02-dialect-default-and-fallback.md) | Default responses + fallback 404/405 + ghi nhớ | Vừa | Xong |
| [03](phase-03-request-record-dialect.md) | `RequestRecord.Dialect` + UI usage | Thấp | Xong |
| 04 | Dropdown 3 lựa chọn + i18n + default | Thấp | Chưa làm |
| 05 | Đối chiếu 3 dialect với spec chính thức | Vừa | Chưa làm |
| 06 | Điều tra `EADDRNOTAVAIL` / `http2` header timeout | **Chưa rõ** | Chưa làm |

## Thứ tự bắt buộc

Phase 04 phụ thuộc 01. Phase 02 và 03 độc lập, song song được.
Phase 05 chạy sau 01–02. Phase 06 độc lập hoàn toàn — điều tra, không phải tính năng.

## Bằng chứng đo được (2026-09-15)

Test trực tiếp qua OmniProxy (`127.0.0.1:8080`), account `TAINGUYENVIBE40KTQ` @ apiforcode,
model `deepseek-v4-pro-0813`:

- `tool_calls` trả **đúng** ở cả 4 đường: chat non-stream, chat stream, history 8 vòng tool, `/v1/messages`.
- Prompt 20.710 token → 6.5s. Hội thoại 3 lượt (1.023 token) → 14.8s.
- Log có 4 lần failover của account này: 2× `read: can't assign requested address`,
  2× `http2: timeout awaiting response headers` → `recording cooldown`.

**Kết luận:** không reproduce được lỗi "mất tool" ở tầng dịch. Nghi vấn còn lại nằm ở
hành vi model TQ phía upstream, và ở request chết giữa dòng do lỗi mạng (Phase 06).

## Global Constraints

- Không thêm dependency Go mới.
- `go build ./...` và `go vet ./...` phải sạch sau mỗi phase.
- **Không đổi hành vi account đang chạy**: dialect rỗng vẫn đi chat (D1).
- File code mới dưới 200 dòng, một trách nhiệm rõ ràng.
- Comment giải thích "tại sao", không "cái gì" — theo văn phong repo.
- `go test ./proxy/` cần `sandbox.network.allowLocalBinding`, nếu không panic khi bind port.
- Commit conventional, không tham chiếu AI.
- Soi `git diff --cached` trước mỗi commit.
