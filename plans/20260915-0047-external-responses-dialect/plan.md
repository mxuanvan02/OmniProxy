# External OpenAI Responses API Dialect — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cho phép account `external_openai` nói Responses API ra upstream, chọn được khi add key, mặc định vẫn là Chat Completions.

**Architecture:** Tách phần dịch request/parse response Responses (đang nằm trong `external_codex.go`) thành helper dùng chung ở `proxy/responses_upstream.go`, có tham số hóa 3 hành vi Codex-specific. Adapter mới `CallExternalOpenAIResponses` tái dùng transport của external (identity headers, WAF fallback, blank-output gate) và gọi helper đó. `dispatchChat` rẽ nhánh theo field `ExternalAPIDialect` trên account.

**Tech Stack:** Go (không thêm dependency), `net/http`, `httptest` cho test, admin UI thuần JS trong `web/`.

**Spec:** `plans/20260915-0047-external-responses-dialect/design.md`

## Global Constraints

- Không thêm dependency Go mới.
- File code dưới 200 dòng nếu tách được; file mới phải có một trách nhiệm rõ ràng.
- Tên file kebab-case hoặc snake_case theo đúng convention package hiện có.
- Mọi thay đổi phải giữ `go build ./...` và `go vet ./...` sạch.
- **Không đổi hành vi account Codex đang chạy.** Test byte-identical ở Phase 01 là cổng chặn.
- Comment giải thích "tại sao", không giải thích "cái gì" — theo văn phong hiện có của repo.
- `go test ./proxy/` cần `sandbox.network.allowLocalBinding` (test bind port sẽ panic trong sandbox mặc định).
- Commit theo conventional commit, không tham chiếu AI.

## Task Index

| Phase | Nội dung | Rủi ro | Trạng thái |
|---|---|---|---|
| [01](phase-01-shared-responses-helpers.md) | Tách helper dùng chung + test byte-identical | **Cao** | Chưa làm |
| [02](phase-02-dialect-config-and-dispatch.md) | Config field, setter, `externalAPIDialect`, rẽ nhánh `dispatchChat` | Thấp | Chưa làm |
| [03](phase-03-external-responses-adapter.md) | Adapter `CallExternalOpenAIResponses` | Vừa | Chưa làm |
| [04](phase-04-admin-ui-and-add-key.md) | Field ở API add key + dropdown UI + i18n | Thấp | Chưa làm |
| [05](phase-05-integration-verification.md) | Hồi quy toàn bộ + đo thật trên gateway | Vừa | Chưa làm |

## Thứ tự bắt buộc

Phase 01 làm **trước tiên** vì nó là thay đổi rủi ro cao nhất (đụng code Codex đang chạy). Phase 02–04 độc lập với nhau sau khi 01 xong. Phase 05 chạy cuối.

## Điểm khác biệt so với design doc

**1. Thêm override `ResponsesPath`.** Design doc §5.4 nói path cố định `/v1/responses`.
Plan này thêm một override `ResponsesPath` trên account, song song với `ChatPath` đã có.

**Lý do:** mục tiêu thật là gateway kiểu Codex (`token.vietshare.site/codex-api`).
Các gateway đó thường phục vụ Responses ở path riêng (`.../codex/responses`), không
phải `/v1/responses`. Không có override thì tính năng nhiều khả năng không chạy được
với chính provider đã thúc đẩy nó. `ChatPath` tồn tại vì đúng lý do này.

Nếu bác muốn cắt cho gọn, bỏ Task 2.1 và 4.3 — phần còn lại vẫn hoạt động, chỉ là
path cố định `/v1/responses`.

**2. Đổi một chuỗi lỗi ở Phase 01.** `"codex SSE stream ended before
response.completed"` → `"SSE stream ended before response.completed"`. Sau khi hàm parse
thành dùng chung, chữ `codex` sai ngữ cảnh với gateway không phải Codex. Chỉ đổi văn bản
lỗi, không đổi control flow; golden test không phủ chuỗi lỗi.

**3. Test stream cụt nằm ở Task 3.4, không phải Phase 05.** Spec §9 yêu cầu test này;
đặt cạnh adapter để nó chạy cùng lúc với phần được kiểm.

**4. Bỏ `SetAccountExternalAPIDialect`.** Design doc §5.1 yêu cầu setter này. Đã kiểm
chứng và bỏ:

- `externalApiDialect` là field **do người dùng nhập**, không phải phát hiện lúc chạy.
  Mọi field người dùng nhập khác đi qua `apiUpdateAccount` (`proxy/handler.go:8148`) bằng
  cách gán trực tiếp `existing.Field = ...`, không qua setter nào.
- Setter không thể nối vào `apiUpdateAccount` mà không hỏng: hàm đó giữ một bản sao
  `existing` rồi persist bằng `config.UpdateAccount(id, *existing)`, và hàm này **thay
  toàn bộ** record (`cfg.Accounts[i] = account`, `config/config.go:1345`). Setter ghi vào
  `cfg.Accounts` trước đó sẽ bị chính bản sao cũ ghi đè.
- Nối vào đường add key thì phải ghi đĩa hai lần (`AddAccount` đã `Save()`) và có một
  khoảng thời gian account tồn tại với dialect rỗng.
- Setter tương tự được spec viện dẫn, `SetAccountExtBillingLimitIsTotal`
  (`config/config.go:1405`), tồn tại cho một đường **phát hiện lúc chạy**
  (`proxy/handler.go:2734`) — nơi không có bản sao `existing` nào đang bay. Analogy không
  đúng. (`SetAccountEnabled`, `config/config.go:1454`, thậm chí không có caller nào.)

Thay vào đó field được ghi trực tiếp lên account literal lúc add key, và Task 2.1 kiểm
bằng test round-trip JSON trên đĩa (`config/account_dialect_test.go`) — chính cái tên key
`externalApiDialect` / `responsesPath` mới là hợp đồng giữa UI và handler.

**Giới hạn phạm vi có chủ đích:** dialect chỉ đặt được lúc add key, không sửa được sau.
Bác yêu cầu "chọn khi add key", nên không mở thêm đường edit. Hệ quả: chọn sai dialect thì
phải xoá và add lại account. Nếu muốn sửa được sau, việc cần làm là thêm một nhánh gán
trực tiếp trong `apiUpdateAccount` — khoảng 5 dòng, không cần setter.

## Self-review đã chạy

- Đối chiếu spec §5.1–§5.5, §7, §8, §9: mọi mục đều có task.
- Quét placeholder: sạch.
- Đối chiếu tên hàm/field giữa các phase với code thật: `externalAuthMethod`,
  `accountLabel`, `truncateErrBody`, `openAICompatibleEndpoint`, `GetClientForProxy`,
  `ResolveAccountProxyURL`, `newBlankOutputGate`, `isExternalAccount`, `dispatchChat`,
  `ChatPath`/`ExternalHeaderProfile` (`config.go:148,153`), `Save()` (`config.go:973`),
  `SetAccountExtBillingLimitIsTotal` (`config.go:1405`) — tất cả đã xác nhận tồn tại.
- Sửa 4 chỗ sai sau khi đối chiếu: helper cô lập config (`config.Init` + file tạm, không
  phải `resetConfigForTest`), cách dựng Handler trong test Phase 04
  (`setupResponsesTestHandler`, không phải `newTestHandler`), `proxy/testdata/` chưa tồn
  tại (test phải tự `MkdirAll`), và số lần dùng `newCodexCoalescer` là 8 không phải 4.
- Vòng quét pre-flight của SDD (2026-09-15) tìm thêm 4 vấn đề và đã xử lý: (a) Task 1.2 và
  1.3 không tự test được vì cây không build — gộp thành một dispatch; (b) setter
  `SetAccountExternalAPIDialect` là dead code — bỏ (deviation #4); (c) claim class `hidden`
  tồn tại ở `web/` — **đã xác minh đúng**, `web/styles.css:3530`, không cần sửa; (d) WIP
  chưa commit có sẵn trong repo sẽ bị cuốn vào commit của plan — implementer phải soi
  `git diff --cached` trước mỗi commit.
