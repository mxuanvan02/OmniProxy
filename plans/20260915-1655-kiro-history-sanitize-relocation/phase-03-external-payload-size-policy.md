# Phase 03 — Chính sách kích thước payload cho external

**Ưu tiên:** Thấp · **Trạng thái:** Chưa làm · **Phụ thuộc:** phase 02
**Loại:** quyết định + thực thi nhỏ

## Bối cảnh

Hôm nay `truncatePayloadToLimit` (`translator.go:1770`) chạy trong **cả hai** translator,
nên **mọi** account — kể cả external — đều được cắt về `maxPayloadBytes = 900KB`
(`translator.go:64`). Nhưng nó cắt trên history **đã sanitize**, tức bản đã co ~1000 lần
trong fixture đo được, nên thực tế **gần như không bao giờ fire** cho external.

Phase 02 bỏ lời gọi đó khỏi translator. External **mất lưới an toàn**, trong khi payload
của nó **to hơn** (giữ đủ tool history có cấu trúc). Đây là đánh đổi cần chốt.

## Vì sao con số 900KB vốn đã sai cho external

`maxPayloadBytes` đo `payloadByteSize(payload)` — kích thước JSON của **`KiroPayload`**.
Wire body gửi tới gateway external là hình dạng **khác** (`kiroPayloadToOpenAIRequest` /
`kiroPayloadToResponsesRequest` / `kiroPayloadToAntigravityRequest`), nên 900KB của
KiroPayload không phải là ngân sách của thứ thực sự được gửi đi. Giới hạn thật của
upstream cũng khác nhau và phần lớn tính theo **token**, không theo byte.

Nên "external mất guard" nghe nghiêm trọng hơn thực tế: guard đó đang đo sai đại lượng.

## Lựa chọn

| | Cách | Đánh đổi |
|---|---|---|
| **(i)** | **Không guard.** Để upstream tự trả lỗi khi payload quá lớn. | Đơn giản nhất, đúng YAGNI. Request quá lớn fail với lỗi rõ từ upstream thay vì bị cắt im lặng. Rủi ro: fail muộn, tốn một lượt gọi. |
| (ii) | Giữ một guard chung, budget riêng cho external | An toàn hơn, nhưng phải chọn số — và không có dữ liệu nào để chọn đúng. Đoán sai thì hoặc vô dụng hoặc cắt nhầm. |
| (iii) | Guard theo dialect (chat / responses / anthropic) | Chính xác nhất, nhiều việc nhất, cần số liệu thật của từng gateway. |

## Đề xuất: (i)

Lý do:

1. **Không có bằng chứng nào cho thấy external đang gặp lỗi kích thước.** Log hiện có
   (`data/omniproxy.launchd.err.log`) ghi 276 `unclassified failure` — toàn bộ là lỗi
   transport và header timeout, không có ca nào là payload quá lớn.
2. Guard hiện tại đo **sai đại lượng** cho external (KiroPayload byte, không phải wire byte).
3. Cắt im lặng là hành vi xấu hơn fail rõ ràng: người vận hành không biết mình vừa mất
   history.
4. YAGNI — thêm một cơ chế không có dữ liệu để hiệu chỉnh là thêm một thứ phải bảo trì.

## Yêu cầu nếu chọn (i)

### Chức năng
1. External **không** bị cắt history. Payload đi nguyên.
2. Khi upstream trả lỗi kích thước, log **một dòng WARN** có account + model + byte size
   của wire body, để lần sau có số liệu mà quyết định (ii)/(iii).

### Phi chức năng
- Không thêm đường code mới nào chạy trong request path ngoài một phép đo khi **đã** lỗi.

## Thiết kế

Trong các adapter external, khi nhận lỗi 400/413 có dấu hiệu kích thước
(`context_length`, `too large`, `max_tokens`, `payload`), log:

```
[ExternalPayload] size-exceeded account=<label> model=<id> bytes=<n> dialect=<d>
```

Cạnh `externalWAFBlocked` (`external_openai.go:100`) theo cùng văn phong. Chỉ log, không
đổi luồng.

## File liên quan

**Sửa:**
- `proxy/external_openai.go` — nhận diện + log, cạnh `externalWAFBlocked:100`
- các adapter external khác nếu cần cùng dòng log

**Tạo:** không

## Todo

- [ ] Helper nhận diện lỗi kích thước từ body upstream
- [ ] Log WARN kèm byte size thật của wire body
- [ ] Test: body lỗi kích thước → log, **không** đổi luồng
- [ ] Test: lỗi khác (401/402/403) **không** bị nhận nhầm
- [ ] `go build ./... && go vet ./...`

## Tiêu chí hoàn thành

- Một request vượt giới hạn upstream fail với lỗi gốc, kèm một dòng log đủ số liệu.
- Không có cắt im lặng nào cho external.

## Rủi ro

| Rủi ro | Giảm thiểu |
|---|---|
| Gateway thật sự cần guard | Log cho số liệu; bật (ii) sau, khi đã biết ngưỡng thật |
| Nhận nhầm lỗi khác thành lỗi kích thước | Chỉ khớp marker cụ thể trong body, không khớp status đơn thuần |

## Bảo mật

Log chỉ chứa số byte + account label + model — **không** nội dung message, **không** key.

## Bước tiếp theo

Nếu log cho thấy có ca thật, mở phase mới làm (ii). Nếu không, đóng ở đây.
