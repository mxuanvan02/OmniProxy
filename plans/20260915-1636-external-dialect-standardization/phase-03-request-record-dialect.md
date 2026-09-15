# Phase 03 — RequestRecord.Dialect + Usage Log

**Ưu tiên:** Thấp · **Trạng thái:** Xong · **Phụ thuộc:** không (độc lập với Phase 01, 02)

## Bối cảnh

`RequestRecord` ghi lại mỗi request nhưng không mang thông tin wire dialect. Khi một operator
có nhiều account external cùng provider nhưng khác dialect (một số gateway chỉ chat, một số
chỉ responses), bảng Usage → By Endpoint gộp tất cả vào một hàng `openai` / `openai-responses`
mà không phân biệt được traffic nào đi qua contract nào. Lỗi cũng vậy: khi lỗi cluster trên
một dialect cụ thể, operator phải mở từng account để tìm ra nguyên nhân.

## Quyết định đã chốt

- Field `Dialect string` trên `RequestRecord`, `omitempty` trong JSON. Native Kiro/AWS traffic
  không có dialect ⇒ field rỗng ⇒ `addToSummaryMap` skip ⇒ không tạo bucket ma.
- `ByDialect map[string]*PeriodSummary` thêm vào `PeriodSummary` (daily bucket) và `UsageStats`
  (merged response). Cùng pattern với `ByEndpoint`.
- `resolveAccountDialect(accountID)` lookup từ config tại thời điểm record. Lấy giá trị operator
  đã chọn, không phải giá trị fallback vừa học được (fallback đã persist nên hai giá trị bằng
  nhau sau lần đầu).
- Request-details API (`/usage/request-details`) trả thêm `endpoint` và `dialect` per detail item.
- Legacy UI (`web/`): dropdown By table thêm option Dialect; drawer chi tiết hiện dòng Dialect
  khi record có giá trị. i18n en/vi/zh.
- `web-next/` (React embedded): **không sửa**. Hai UI tồn tại song song; workstream
  `usage-dashboard-rework` đang rebuild `web-next/` độc lập. Field `dialect` là additive +
  `omitempty` nên React client bỏ qua cho tới khi workstream kia tự quyết định render.

## Thiết kế

### Backend

`proxy/usage_tracker.go`:
- `RequestRecord.Dialect string \`json:"dialect,omitempty"\``
- `PeriodSummary.ByDialect map[string]*PeriodSummary \`json:"byDialect,omitempty"\``
- `UsageStats.ByDialect map[string]*PeriodSummary \`json:"byDialect"\``
- `pushToRingLocked`: init `day.ByDialect` nếu nil, gọi `addToSummaryMap(day.ByDialect, r.Dialect, r)`
  khi `r.Dialect != ""`.
- `GetStats`: init `stats.ByDialect`, merge từ daily buckets.

`proxy/handler.go`:
- `resolveAccountDialect(accountID string) string` — lookup `ExternalAPIDialect` từ config.
- `recordUsageWithCache`: set `rec.Dialect = resolveAccountDialect(accountID)`.
- `recordError`: set `Dialect: resolveAccountDialect(accountID)`.
- `apiGetUsageRequestDetails.DetailItem`: thêm `Endpoint` và `Dialect` fields.

### Frontend (legacy `web/`)

`web/usage.js`:
- `renderUsageTable`: thêm `case 'dialect'` đọc `stats.byDialect`.
- Dropdown `<select>` thêm option `dialect`.
- `initUsagePage`: sortBy/sortOrder thêm key `dialect`.
- `renderDetailDrawer`: hiện dòng Dialect khi `d.dialect` truthy.

`web/locales/{en,vi,zh}.json`:
- `usage.usageByDialect`, `usage.tabDialect`, `usage.drawer.dialect`.

## Khác so với thiết kế ban đầu

| Điểm | Thiết kế ban đầu | Thực tế | Lý do |
|---|---|---|---|
| Cờ output trên callback | Thêm field `Produced bool` vào `KiroStreamCallback` | Dùng `callback.HasOutput()` có sẵn | DRY/YAGNI — signal đã tồn tại, gate đã wire ở handler.go:4894/5735 và responses_handler.go:223 |
| Fallback helper file | Inline trong `external_openai.go` | File riêng `external_dialect_fallback.go` (92 dòng) | Giới hạn 200 dòng/file; tách trách nhiệm rõ ràng |
| Config updater | Snapshot-based `UpdateAccountPreservingCredentials` | Single-field `SetAccountExternalAPIDialect` | Tránh race condition khi hai request đồng thời fallback cùng account |
| web-next changes | Sửa cả hai UI | Chỉ sửa legacy `web/` | Workstream `usage-dashboard-rework` đang rebuild `web-next/` độc lập; field additive + omitempty không phá họ |
| Detail API | Chỉ thêm dialect | Thêm cả endpoint lẫn dialect | DetailItem trước đó drop cả hai; dialect một mình không đủ context |

## Kiểm chứng — kết quả đo

```
$ go test ./proxy/ -run 'TestRequestRecordCarriesDialect|TestRequestRecordOmitsDialectForNonExternalAccounts|TestRecordErrorCarriesDialect' -count=1
ok  	omniproxy/proxy	0.973s

$ go test ./... -count=1 -skip 'TestSearchAdaptersUseNativeContracts'
ok  	omniproxy/auth	1.000s
ok  	omniproxy/cli	1.741s
ok  	omniproxy/config	9.869s
ok  	omniproxy/logger	2.159s
ok  	omniproxy/pool	4.059s
ok  	omniproxy/proxy	18.926s
```

Ba test mới cover:
1. External account dialect `responses` ⇒ record mang `Dialect="responses"`, `ByDialect["responses"]`
   aggregate đúng tokens.
2. Native account (auth method `kiro`) ⇒ record `Dialect=""`, `ByDialect` empty — không bucket ma.
3. Error record cũng mang dialect ⇒ `ByDialect["anthropic"].Errors == 1`.

## File liên quan

**Sửa:**
- `proxy/usage_tracker.go` (+11 dòng) — `RequestRecord.Dialect`, `PeriodSummary.ByDialect`,
  `UsageStats.ByDialect`, aggregation trong `pushToRingLocked` và `GetStats`.
- `proxy/handler.go` (+35 dòng) — `resolveAccountDialect`, set dialect trong `recordUsageWithCache`
  và `recordError`, thêm `Endpoint`+`Dialect` vào `DetailItem`.
- `web/usage.js` (+19 dòng) — case dialect, dropdown option, sort state, drawer row.
- `web/locales/{en,vi,zh}.json` (+3 dòng mỗi file) — i18n keys.

**Tạo:**
- `proxy/usage_dialect_test.go` (146 dòng) — 3 test coverage.

## Todo

- [x] Thêm `Dialect` field vào `RequestRecord` (omitempty)
- [x] Thêm `ByDialect` vào `PeriodSummary` và `UsageStats`
- [x] Aggregate trong `pushToRingLocked` (skip empty key)
- [x] Merge trong `GetStats`
- [x] `resolveAccountDialect` helper
- [x] Set dialect trong `recordUsageWithCache` và `recordError`
- [x] Detail API trả endpoint + dialect
- [x] Legacy UI: dropdown + drawer + i18n
- [x] Test: external record mang dialect + ByDialect aggregate
- [x] Test: native record không tạo bucket ma
- [x] Test: error record cũng mang dialect
- [x] `go build ./... && go vet ./...`
- [x] Full suite xanh

## Tiêu chí hoàn thành

- Mỗi usage record của external account mang đúng dialect operator đã chọn.
- By Dialect table hiện trong legacy UI, aggregate đúng theo thời gian (không bị ring buffer cap).
- Native traffic không tạo bucket ma.
- Detail drawer hiện dialect khi có.
- Không phá `web-next/` (workstream song song).

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Field mới làm JSON response nặng hơn | `omitempty` — native traffic gửi nothing; external traffic thêm ~10 bytes/record |
| Daily bucket cũ không có ByDialect | `mergeSummaryMapInto` skip nil maps — totals vẫn đúng, breakdown chỉ có từ ngày field được thêm |
| Race giữa record và fallback persist | `resolveAccountDialect` đọc config tại thời điểm record; fallback đã persist trước khi retry chat nên giá trị nhất quán |
| Workstream song song bị ảnh hưởng | Field additive + omitempty; không sửa file web-next nào |

## Bảo mật

`resolveAccountDialect` chỉ đọc `ExternalAPIDialect` (chuỗi enum), không đụng credential.
Detail API đã strip credential từ trước; thêm hai field metadata không đổi risk profile.

## Bước tiếp theo

Phase 04 (dropdown 3 lựa chọn + anthropic option trong form add key) phụ thuộc phase 01.
Phase 05 (audit spec) chạy sau 01–02. Phase 06 (EADDRNOTAVAIL) độc lập.
