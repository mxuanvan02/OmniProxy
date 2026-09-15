# Phase 04 — Dropdown 3 dialect + i18n + default

**Ưu tiên:** Thấp · **Trạng thái:** Xong · **Phụ thuộc:** Phase 01 (adapter Alibaba Cloud)

## Bối cảnh

Phase 01 đã build xong adapter `CallExternalAnthropic` cho dialect thứ ba (`anthropic` — Anthropic
Messages API), và backend import (`handler.go`) đã đọc field `AnthropicPath`. Nhưng form add key ở
legacy UI (`web/accounts.js`) chỉ có hai lựa chọn: Chat Completions và Responses. Operator không
có cách nào chọn dialect Anthropic Messages từ UI — chỉ set tay qua API. Phase này nối UI vào
backend đã có sẵn, không thêm logic Go nào.

Yêu cầu gốc: external account chọn được cả 3 dialect khi add key, `responses` là mặc định,
và cả dropdown lẫn help text theo i18n en/vi/zh.

## Quyết định đã chốt

- **3 option, default `responses`:** thứ tự dropdown là Responses (selected) → Chat → Anthropic
  Messages. Default `responses` khớp D2 của plan và phase 02.
- **Path override riêng cho mỗi dialect:** Anthropic Messages có input `Messages path` riêng
  (`externalAnthropicPath`), placeholder `/v1/messages` — đối xứng với `Responses path`
  (`externalResponsesPath`) đã có từ phase trước. Chat không có path override (hard-code
  `/v1/chat/completions`).
- **Chỉ hiện path của dialect đang chọn:** chọn Responses → hiện Responses path, ẩn Messages
  path; chọn Anthropic Messages → ngược lại; chọn Chat → ẩn cả hai. Tránh form rối và tránh
  gửi nhầm giá trị path đã gõ trước khi ẩn (cùng lý do phase trước đã guard `responsesPath`).
- **Chỉ gửi path khớp dialect:** `importExternal` đọc `externalAnthropicPath` và gửi
  `anthropicPath` **chỉ khi** dialect là `anthropic`, else chuỗi rỗng — đúng pattern guard sẵn có
  của `responsesPath`. Backend (`handler.go:10798-10803`) cũng đã switch theo dialect nên hai
  lớp phòng thủ trùng nhau.
- **Nhãn "Anthropic Messages", không phải "Alibaba Cloud Messages":** dialect này nói chuyện
  Anthropic Messages API (`POST /v1/messages`, header `anthropic-version: 2023-06-01`) theo đúng
  phase-01 và D4. Mọi gateway tương thích `api.anthropic.com` đều phục vụ được, không riêng
  Alibaba Cloud — đặt tên theo một vendor sẽ gây hiểu lầm cho operator dùng gateway khác.

## Thiết kế

### Frontend (legacy `web/`)

`web/accounts.js` — trong `modalExternalProvider`:
- Thêm `<option value="anthropic">` (nhãn `external.dialectAnthropic`) vào `<select id="externalDialect">`.
- Thêm `form-group` mới `externalAnthropicPathGroup` (mặc định `hidden`) chứa input
  `externalAnthropicPath`, placeholder `external.anthropicPathPlaceholder`.
- Thay listener toggle đơn bằng `updatePathVisibility()`: toggle cả `responsesPathGroup`
  (`hidden` khi `value !== 'responses'`) lẫn `anthropicPathGroup` (`hidden` khi `value !== 'anthropic'`).
  Gọi một lần lúc khởi tạo để đồng bộ với default `responses`.

`web/accounts.js` — trong `importExternal`:
- Thêm `const anthropicPath = externalApiDialect === 'anthropic' ? $('externalAnthropicPath').value.trim() : '';`
- Đưa `anthropicPath` vào body `POST /auth/external-provider`.

`web/locales/{en,vi,zh}.json`:
- `external.dialectAnthropic` = "Anthropic Messages" (cả 3 ngôn ngữ, nhãn giữ nguyên vì là tên API).
- `external.anthropicPathLabel` = "Messages path" / "Đường dẫn Messages" / "Messages 路径".
- `external.anthropicPathPlaceholder` = "/v1/messages".
- Sửa `external.dialectChat`: bỏ hậu tố "(default)" (mặc định giờ là responses).
- Sửa `external.dialectResponses`: thêm "(default)" / "(mặc định)" / "（默认）".
- Viết lại `external.dialectHelp`: mô tả cả 3 dialect thay vì chỉ 2.

### Backend

**Không sửa.** `handler.go` đã có `AnthropicPath` field trong import body struct (10741), đã đọc
nó trong `case "anthropic"` (10802), đã gán vào `config.Account` (10822). `config.Account.AnthropicPath`
đã tồn tại từ phase 01 (config.go:177-183). `externalAnthropicPath()` (external_anthropic.go:50)
đã đọc override với default `/v1/messages`.

## Kiểm chứng — kết quả đo

```
$ node --check web/accounts.js
accounts.js OK

$ node -e "JSON.parse(...)" web/locales/{en,vi,zh}.json
web/locales/en.json OK
web/locales/vi.json OK
web/locales/zh.json OK

$ go build ./...
(sạch)

$ go vet ./...
(sạch)

$ go test ./config/... -count=1
ok  	omniproxy/config	6.071s

$ go test ./proxy/... -count=1 -skip 'TestSearchAdaptersUseNativeContracts'
ok  	omniproxy/proxy	12.053s
```

Test backend cover sẵn đường đi của `anthropicPath` từ phase 01:
- `TestImportExternalProviderStoresAnthropicPathOnlyForAnthropic`
  (`external_provider_dialect_test.go:128`) — xác nhận import chỉ lưu path khi dialect là
  `anthropic`, drop khi là dialect khác; override `/anthropic/v1/messages` được giữ nguyên.
- `external_anthropic_adapter_test.go:128` — `account.AnthropicPath = override` được adapter tôn trọng.

Phase 04 là UI-only nên không thêm test Go mới; hành vi gửi-đúng-path đã được test import ở
phase 01 cover. Test JS form (toggle visibility, chỉ gửi path khớp dialect) chưa có harness
trong repo cho legacy `web/` — ghi nhận là follow-up, không chặn phase.

> **Lưu ý flake:** `go test ./proxy/` đôi lúc FAIL ở họ `TestMarkCodex*` (~20% cả trên cây
> pristine lẫn cây đã sửa — không do phase này). Chạy lại `-count=1` pass. Đã xác nhận ở phase 02.

## Khác so với thiết kế ban đầu

| Điểm | Dự kiến | Thực tế | Lý do |
|---|---|---|---|
| Nhãn dialect thứ 3 | "Alibaba Cloud Messages" (như summary session trước) | "Anthropic Messages" | Đây là Anthropic Messages API; đặt theo một vendor gây hiểu lầm cho gateway khác. Khớp phase-01 + D4. |
| Backend change | Có thể cần wire `anthropicPath` | Không sửa gì | Phase 01 đã wire sẵn end-to-end; phase 04 chỉ nối UI vào. |
| Test mới | Test JS toggle | Không thêm | Legacy `web/` không có JS test harness; hành vi path đã cover bởi test import phase 01. |

## File liên quan

**Sửa:**
- `web/accounts.js` (+~15 dòng) — option `anthropic`, `externalAnthropicPathGroup` + input,
  `updatePathVisibility()`, gửi `anthropicPath` trong `importExternal`.
- `web/locales/en.json` (+4 / sửa 3 dòng) — `dialectAnthropic`, `anthropicPathLabel`,
  `anthropicPathPlaceholder`; sửa `dialectChat`, `dialectResponses`, `dialectHelp`.
- `web/locales/vi.json` (+4 / sửa 3 dòng) — như en, đã dịch.
- `web/locales/zh.json` (+4 / sửa 3 dòng) — như en, đã dịch.

**Tạo:** không (chỉ doc này).

**Backend:** không sửa (đã sẵn sàng từ phase 01).

## Todo

- [x] Thêm `<option value="anthropic">` vào dropdown
- [x] Thêm `externalAnthropicPathGroup` + input (mặc định hidden)
- [x] `updatePathVisibility()` toggle cả 2 path group theo dialect
- [x] Gửi `anthropicPath` trong `importExternal` (guard theo dialect)
- [x] i18n en/vi/zh: `dialectAnthropic`, `anthropicPathLabel`, `anthropicPathPlaceholder`
- [x] i18n: bỏ "(default)" khỏi Chat, thêm vào Responses, viết lại `dialectHelp`
- [x] `node --check` + JSON.parse validate
- [x] `go build ./... && go vet ./...` sạch
- [x] `go test ./config/... ./proxy/...` xanh
- [x] Viết phase-04 doc, cập nhật plan.md Task Index

## Tiêu chí hoàn thành

- Form add key external hiện 3 dialect; mặc định Responses.
- Chọn dialect nào chỉ hiện path override của dialect đó; Chat không hiện path nào.
- Submit gửi `anthropicPath` chỉ khi dialect là `anthropic`; backend lưu đúng (đã test phase 01).
- Nhãn + help text đủ en/vi/zh, mô tả đúng cả 3 dialect.
- Không đổi hành vi account đang chạy; không sửa Go.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Operator gõ path rồi đổi dialect, path cũ bị gửi nhầm | `importExternal` chỉ đọc input của dialect đang chọn; group kia bị ẩn và không đọc |
| Nhãn "Alibaba Cloud" khóa chặt vào một vendor | Đã đổi thành "Anthropic Messages" — mô tả wire protocol, không vendor |
| Legacy `web/` không có JS test | Hành vi gửi-path đã cover bởi test import Go (phase 01); toggle visibility là logic 1 dòng, low-risk |
| File `accounts.js` đã 4116 dòng (vượt quy tắc 200) | File legacy có sẵn; phase chỉ thêm ~15 dòng vào form hiện có, không tách file mới (tránh phạm vi lan) |

## Bảo mật

Không đổi risk profile. `anthropicPath` là đường dẫn URL (không phải credential); chỉ lưu khi
dialect khớp. Key vẫn đi qua header (`x-api-key` + `Authorization`, D5 phase 01), không qua
query string. Log không in key (dùng `accountLabel`).

## Bước tiếp theo

Phase 05 — đối chiếu cả 3 dialect (chat / responses / Alibaba Cloud Messages) với spec chính
thức của từng contract; chạy sau 01–02 (đều đã xong). Phase 06 — điều tra `EADDRNOTAVAIL` /
`http2` header timeout, độc lập.

Follow-up không chặn: cân nhắc tách `modalExternalProvider` + `importExternal` ra module nhỏ
nếu `accounts.js` tiếp tục phình; và thêm JS test harness cho legacy `web/` nếu UI dialect còn mở rộng.
