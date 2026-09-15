# Phase 05 — Hồi quy toàn bộ và đo thật trên gateway

**Priority:** P1
**Trạng thái:** Chưa làm
**Ước lượng:** ~3 giờ (1 giờ hồi quy + 2 giờ đo, phụ thuộc gateway)
**Phụ thuộc:** Phase 01 → 04 xong hết

## Context Links

- Spec: [`design.md`](design.md) §8, §9
- Changelog: `CHANGELOG.md` — mục `## [Unreleased]` → `### Added`
- Chạy lại proxy: `./restart.sh` (KHÔNG chạy `./omniproxy` tay — `CheckAndKillExisting`
  trong `main.go` giết bản đang chạy)
- Endpoint đo: `GET /usage/request-details` (`proxy/handler.go:12740`), `GET /stats`
- Gateway thật: `https://token.vietshare.site/codex-api`

## Overview

Chốt lại: chạy hồi quy toàn repo để chắc Phase 01 không làm hỏng gì, kiểm tĩnh rằng stub
đã biến mất, rồi đo thật hai dialect trên gateway đã thúc đẩy tính năng này.

## Key Insights

- **Phase 01 là rủi ro duy nhất ảnh hưởng account đang chạy.** Các phase sau chỉ *thêm*.
  Nên hồi quy ở đây tập trung vào: build/vet sạch, và nhóm test Codex/Responses xanh.
- **Đo được gì là câu hỏi mở, không phải điều đã biết trước.** Proxy gửi full history mỗi
  lượt (client Claude gửi vậy), `store: false` và không có `previous_response_id`, nên
  **reasoning carry-over không xảy ra** ở phạm vi này. Thứ có thể khác biệt chỉ là
  `cached_tokens` do prompt cache phía gateway. Nếu số đo không khác, kết luận đúng là
  "dialect này cần thiết vì gateway chỉ nói Responses", không phải "tiết kiệm token".
  Ghi thẳng kết quả đo được, kể cả khi nó là 0 khác biệt.
- **Giá trị thật của phase này là chứng minh dialect chạy được với gateway thật.** Một
  gateway resale kiểu Codex có thể từ chối chat completions hoặc dịch mất mát; đó là lý
  do tính năng tồn tại.
- **`/usage/request-details` cần token admin.** Route admin nay yêu cầu `X-Admin-Token`
  (`CHANGELOG.md` mục Security) — lấy token qua `POST /admin/api/login`, hoặc đọc số
  usage ngay trong response của chính request đo.

## Requirements

**Functional**
- `go build ./...`, `go vet ./...` sạch.
- `go test ./...` xanh toàn bộ.
- Không còn stub: `rg 'not implemented yet' proxy/` không kết quả.
- Không còn tên `codex*` cho các thành phần đã chung hoá trong `responses_upstream.go`.
- Đo được cả hai dialect trên gateway thật, ghi lại số `cached_tokens` /
  `reasoning_tokens` / `input_tokens` / `output_tokens`.

**Non-functional**
- Không đổi code trong phase này trừ khi phát hiện lỗi. Nếu phải sửa, sửa tối thiểu và
  ghi rõ vào commit.

## Architecture

```
1. go build / go vet / go test ./...        → hồi quy Phase 01
2. rg kiểm tĩnh                             → stub + tên cũ đã hết
3. ./restart.sh                             → nạp binary mới
4. add 2 account (chat + responses)         → qua UI hoặc /auth/external-provider
5. curl 127.0.0.1:8080/v1/chat/completions  → 2 lượt cùng prefix lớn, mỗi dialect
6. GET /usage/request-details               → so số
```

## Related Code Files

**Sửa**
- `CHANGELOG.md` — mục `### Added` của `## [Unreleased]`

Không sửa file code nào khác, trừ khi hồi quy lộ lỗi.

## Implementation Steps

### Task 5.1: Hồi quy toàn repo

- [ ] **Step 1: Build và vet**

```bash
cd /Users/van/Tools/OmniProxy && go build ./... && go vet ./... && echo BUILD-VET-OK
```

Kỳ vọng: in `BUILD-VET-OK`, không output lỗi nào khác.

- [ ] **Step 2: Toàn bộ test**

```bash
cd /Users/van/Tools/OmniProxy && go test ./... 2>&1 | tail -30
```

Kỳ vọng: không dòng `FAIL`. Nếu `./proxy/` panic vì bind port, chạy lại với sandbox cho
phép local binding (`sandbox.network.allowLocalBinding`) — đây là hành vi đã biết của
sandbox, không phải lỗi code.

- [ ] **Step 3: Nhóm test Codex/Responses chạy kỹ**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'Codex|Responses|codex' -count=1 -v 2>&1 | tail -40
```

Kỳ vọng: PASS toàn bộ, đặc biệt `TestCodexResponsesBodyGolden` (golden phải khớp, không
được regenerate).

- [ ] **Step 4: Race detector trên nhóm vừa sửa**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ ./config/ -race -count=1 2>&1 | tail -20
```

Kỳ vọng: PASS, không `DATA RACE`.

### Task 5.2: Kiểm tĩnh — stub và tên cũ đã hết

- [ ] **Step 1: Stub Phase 02 không còn**

```bash
cd /Users/van/Tools/OmniProxy && rg -n 'not implemented yet' proxy/ ; echo "exit=$?"
```

Kỳ vọng: không kết quả, `exit=1`. Nếu còn kết quả → Phase 03 chưa thay stub, dừng lại và
làm nốt Phase 03.

- [ ] **Step 2: Tên cũ không còn trong code dùng chung**

```bash
cd /Users/van/Tools/OmniProxy && rg -n 'parseCodexResponsesSSE|processCodexSSELine|codexToolAccum|codexSSELineResult|codexToolChoice|codexMessageContent|codexBufferedReadCloser|parseCodexResponsesJSON' proxy/
```

Kỳ vọng: không kết quả.

- [ ] **Step 3: Wrapper Codex vẫn còn đúng một chỗ**

```bash
cd /Users/van/Tools/OmniProxy && rg -n 'func kiroPayloadToCodexResponsesRequest|func kiroPayloadToResponsesRequest' proxy/
```

Kỳ vọng: đúng 2 dòng — một wrapper ở `external_codex.go`, một hàm chung ở
`responses_upstream.go`.

- [ ] **Step 4: Không đụng `newCodexCoalescer`**

```bash
cd /Users/van/Tools/OmniProxy && rg -n 'newCodexCoalescer' proxy/
```

Kỳ vọng: đúng **8** dòng —

| Số dòng | Nội dung |
|---|---|
| 1 | comment mô tả ở `external_codex.go` |
| 1 | định nghĩa `func newCodexCoalescer` ở `external_codex.go` |
| 3 | lời gọi trong `handler.go` ×2 và `responses_handler.go` ×1 |
| 3 | lời gọi trong `external_codex_test.go` |

Nếu ít hơn 8, có lời gọi bị xoá nhầm khi tách file.

### Task 5.3: Đo thật trên gateway

- [ ] **Step 1: Nạp binary mới**

```bash
cd /Users/van/Tools/OmniProxy && ./build.sh && ./restart.sh
```

Kỳ vọng: script báo health check qua `http://127.0.0.1:8080/v1/models`.

- [ ] **Step 2: Thêm hai account cho cùng một key**

Vào admin UI → Add account → External OpenAI. Thêm hai lần với **cùng** `baseUrl` và
`apiKey` của gateway, đặt tên khác nhau để phân biệt:

| Tên | Dialect | Responses path |
|---|---|---|
| `vietshare-chat` | Chat Completions | (bỏ trống) |
| `vietshare-responses` | Responses API | thử `/v1/responses` trước |

Nếu `/v1/responses` trả 404 ở bước sau, sửa `ResponsesPath` thành đường dẫn gateway công
bố (gateway kiểu Codex thường là `/codex/responses` hoặc `/codex-api/v1/responses`) và
thử lại. Đây chính là lý do override tồn tại.

- [ ] **Step 3: Gửi một request qua dialect chat**

Thay `<CLIENT_KEY>` bằng client key trong admin UI, `<MODEL>` bằng model gateway này phục
vụ (xem `/v1/models`).

```bash
cd /Users/van/Tools/OmniProxy && curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <CLIENT_KEY>" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL>","stream":false,"messages":[{"role":"user","content":"Tra loi dung mot tu: pong"}]}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps(d.get("usage"), ensure_ascii=False)); print(d.get("choices",[{}])[0].get("message",{}).get("content"))'
```

Kỳ vọng: in ra object `usage` và chuỗi trả lời. Ghi lại `usage` vào bảng ở Step 6.

Nếu nhận 4xx/5xx: ghi lại nguyên văn status và body. **Đây là một kết quả đo, không phải
thất bại** — nghĩa là gateway không phục vụ chat completions cho account này.

- [ ] **Step 4: Gửi request y hệt qua dialect responses**

Cách chắc chắn nhất để buộc request đi vào account Responses: tạm tắt (disable) account
`vietshare-chat` trong UI, gửi lại đúng lệnh curl ở Step 3, rồi bật lại.

Kỳ vọng: nội dung trả lời giống Step 3. `usage` có thể khác.

- [ ] **Step 5: Kiểm account nào thực sự phục vụ request**

```bash
cd /Users/van/Tools/OmniProxy && curl -sS 'http://127.0.0.1:8080/usage/request-details?pageSize=5' \
  -H "X-Admin-Token: <ADMIN_TOKEN>" \
  | python3 -c 'import json,sys; [print(r.get("timestamp"), r.get("account"), r.get("provider"), r.get("totalTokens"), r.get("error","")) for r in json.load(sys.stdin).get("items",[])]'
```

Lấy `<ADMIN_TOKEN>` từ `POST /admin/api/login` với mật khẩu admin. Kỳ vọng: thấy email/
nickname account vừa gửi, xác nhận đúng account đã phục vụ.

- [ ] **Step 6: Ghi lại bảng so sánh**

| Dialect | HTTP | input_tokens | cached_tokens | output_tokens | reasoning_tokens | Ghi chú |
|---|---|---|---|---|---|---|
| chat | | | | | | |
| responses | | | | | | |

Kết luận phải nêu rõ một trong ba:

1. **Responses chạy, chat không chạy** → đây là giá trị chính của tính năng; ghi lại.
2. **Cả hai chạy, số token khác nhau** → ghi rõ khác ở đâu và bao nhiêu; đừng suy diễn
   nguyên nhân từ một lần đo, prompt cache cần lượt thứ hai trở đi mới thể hiện.
3. **Cả hai chạy, số token như nhau** → ghi thẳng: chưa đo được lợi ích token ở phạm vi
   này; lợi ích nằm ở chỗ gateway chỉ nói Responses (nếu đúng vậy).

Muốn kiểm prompt cache thì gửi **lượt thứ hai** với cùng một prefix dài (vài nghìn token
giống nhau) và so `cached_tokens` giữa lượt 1 và lượt 2 trên cùng dialect.

### Task 5.4: Changelog và commit

- [ ] **Step 1: Thêm mục vào `CHANGELOG.md`**

Chèn vào `### Added` của `## [Unreleased]` (đầu mục, `CHANGELOG.md:37`):

```markdown
- **Per-account Responses API dialect for external providers.** An `external_openai` account can now send the Responses wire shape instead of chat completions, selectable when the key is added (`externalApiDialect`, plus an optional `responsesPath` for gateways that serve Responses behind a prefix). This is required by gateways whose backend speaks only Responses — a reseller of ChatGPT Codex subscription capacity, for example — where a chat-completions request forces a lossy internal translation. The wire translation is shared with the Codex adapter and now lives in `responses_upstream.go`; Codex behaviour is pinned byte-identical by a golden test. Chat completions remains the default, because most resale gateways serve chat only.
```

Nếu bảng đo ở Task 5.3 cho kết quả cụ thể, thêm **một câu** vào cuối mục nêu đúng số đo
hoặc việc gateway chỉ chạy được một dialect. Không viết "tiết kiệm token" nếu chưa đo được.

- [ ] **Step 2: Commit**

```bash
cd /Users/van/Tools/OmniProxy && git add CHANGELOG.md && git commit -m "docs(changelog): note the per-account Responses API dialect"
```

Nếu Task 5.1–5.3 phải sửa code để hết lỗi, tách commit riêng cho phần sửa đó, thông điệp
nêu rõ lỗi gì.

## Todo List

- [ ] 5.1 Hồi quy: build, vet, `go test ./...`, race
- [ ] 5.2 Kiểm tĩnh: stub hết, tên cũ hết, wrapper đúng 1, coalescer còn 8
- [ ] 5.3 Đo thật trên vietshare: chat vs responses, ghi bảng
- [ ] 5.4 CHANGELOG + commit

## Success Criteria

- `go build ./...`, `go vet ./...`, `go test ./...` đều sạch.
- `TestCodexResponsesBodyGolden` PASS mà **không** regenerate golden.
- `rg 'not implemented yet' proxy/` không kết quả.
- Bảng đo ở Task 5.3 được điền, có kết luận nêu rõ thuộc một trong ba trường hợp.
- `CHANGELOG.md` có mục mô tả, không có tuyên bố tiết kiệm token chưa đo.

## Risk Assessment

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Gateway thật từ chối cả hai dialect (key hết hạn, sai model) | Vừa | Step 3 ghi lại status + body nguyên văn; phân biệt lỗi key với lỗi dialect |
| Đo một lần rồi kết luận về prompt cache | Vừa | Step 6 yêu cầu lượt thứ hai cùng prefix dài |
| Phase 01 lộ lỗi chỉ khi chạy thật, không lộ trong test | Thấp | Golden + nhóm test Codex chạy `-count=1`; nếu account Codex đang chạy bị lỗi, revert commit refactor |
| Đo trên account thật làm tốn credit | Thấp | Request đo là một câu ngắn, không phải prompt dài — trừ lượt kiểm cache có chủ đích |

## Security Considerations

- Không dán `apiKey`, `CLIENT_KEY`, `ADMIN_TOKEN` vào file trong repo, không commit chúng.
  Lệnh curl trong plan dùng placeholder — điền tại chỗ, đừng lưu vào script.
- `curl` gọi `127.0.0.1:8080`, không đẩy dữ liệu ra ngoài.
- Body lỗi từ gateway in ra khi debug có thể chứa thông tin nội bộ — không dán vào
  changelog hay commit message.
- Không đổi cấu hình bảo mật nào ở phase này.

## Next Steps

Hết plan. Nếu Task 5.3 kết luận gateway chỉ nói Responses, việc tiếp theo (ngoài phạm vi)
là cân nhắc reasoning carry-over qua `previous_response_id` — cần state phía proxy, đã
liệt kê trong `design.md` §3 là nằm ngoài phạm vi.
