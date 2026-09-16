# Phase 03 — Ghi chú vận hành

**Ưu tiên:** Thấp · **Trạng thái:** Xong · **Phụ thuộc:** Phase 01, 02

## Context Links

- [plan.md](plan.md), [phase-01](phase-01-alias-core.md), [phase-02](phase-02-handler-wiring.md)

## Đọc log

Alias chỉ log khi rewrite thực sự xảy ra — đường happy path im lặng:

```
INFO [ModelAlias] qwen3.8-max -> qwen3.8-max-cn (exact model unavailable)
```

Grep `[ModelAlias]` để biết request nào đã được cứu và cứu sang variant nào.
Usage log (`RequestRecord.Model`) và request outbound mang **tên variant thật**,
nên số liệu token/chi phí quy đúng về variant đã chạy.

## Escape hatch khi alias chọn sai variant

1. **`ModelMappings` của account**: mapping redirect vẫn thắng và khiến
   `accountHasModel` reject account đó cho tên nguồn (`pool/account.go:530`) —
   cách tắt alias cho một cặp tên cụ thể mà không sửa code.
2. **Thu hẹp catalog**: bỏ variant không mong muốn khỏi danh sách account
   (discovery sẽ không đưa nó vào `modelLists` nữa).
3. **Sửa danh sách suffix**: `aliasLocaleSuffixes` trong `pool/model_alias.go`
   là danh sách cứng 2 locale; thêm/bớt ở một chỗ duy nhất.

## Điều alias KHÔNG làm

- Không gộp variant hành vi: `qwen3.8-max-agent`, `-thinking-agent`, `-fast-agent`,
  `-flash`, effort (`-high`/`-low`) vẫn là tên riêng — agent client tự gọi khi cần.
- Không đổi shape `/v1/models`: client vẫn thấy đủ tên variant raw.
- Không thêm config mới, không thêm dependency.
- Không chạm combo/adaptive routing: combo chứa tên thật, chạy trước alias.

## Kiểm chứng cuối

```
$ go build ./...            # sạch
$ go vet ./proxy/ ./pool/   # sạch
$ go test ./pool/ -count=1  # ok
$ go test ./proxy/ -count=1 # chỉ fail có sẵn TestSearchAdaptersUseNativeContracts (DNS sandbox)
```

## Next Steps

Không có. Theo dõi log `[ModelAlias]` trong vận hành; nếu xuất hiện pattern
suffix mới (vd `-eu`, `-us`) thì thêm vào `aliasLocaleSuffixes` kèm test.
