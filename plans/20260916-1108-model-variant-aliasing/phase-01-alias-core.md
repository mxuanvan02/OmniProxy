# Phase 01 — Alias core trong pool

**Ưu tiên:** Vừa · **Trạng thái:** Xong · **Phụ thuộc:** —

## Context Links

- [plan.md](plan.md) — quyết định D1, D3
- `pool/account.go:522` — `accountHasModel` (exact-match, không đổi)
- `pool/account.go:574` — `normalizeCatalogModelID` (nền cho canonical key)
- `pool/account.go:1062` — `HasAvailableAccountForModel` (mẫu bộ lọc sức khỏe)

## Overview

Thêm lớp canonical key và rescue lookup vào pool mà KHÔNG chạm hàm exact-match hiện có.

## Key Insights

- `normalizeCatalogModelID` đã xử lý case/provider-prefix/`[1m]`/claude dot→dash; canonical key
  chỉ cần strip thêm suffix triển khai (locale, snapshot) trên nền đó.
- Snapshot có thể đi kèm locale (`deepseek-v4-flash-cn-0731`) → strip lặp đến khi bất động.
- Thứ tự candidate phải deterministic: không dựa vào map iteration.

## Requirements

- `canonicalModelKey`: gộp locale/snapshot/case/dash; giữ nguyên suffix hành vi.
- `rankAliasCandidates`: exact(0) > bare(1) > locale theo thứ tự config(2+i) > snapshot mới nhất(5) > khác(8).
- `FindAvailableAliasModel`: union catalog các account, lọc candidate cùng key, trả candidate đầu
  qua bộ lọc sức khỏe (accountHasModel + isModelLocked + cooldown + quota). Trả "" khi không có.
- Không giữ lock sai thứ tự: đọc `config.GetAllowOverUsage()` trước `p.mu.RLock()` như hàm :1062.

## Related Code Files

- Tạo: `pool/model_alias.go` (179 dòng)
- Tạo: `pool/model_alias_test.go` (136 dòng)

## Implementation Steps

1. `aliasLocaleSuffixes = []string{"-cn", "-on"}` + `aliasSnapshotSuffix = -\d{4}$`.
2. `canonicalModelKey` strip lặp; `rankAliasCandidates` sort theo rank/snapshot/locale/name.
3. `FindAvailableAliasModel` dựng candidate từ `p.modelLists`, loại exact, rank, probe sức khỏe.
4. Unit tests: grouping, giữ suffix hành vi, ordering, nil pool/blank request, cooldown đảo ưu tiên.

## Todo List

- [x] canonical key + ordering + pool rescue
- [x] unit tests xanh (`go test ./pool/ -count=1` ok)

## Success Criteria

- `TestCanonicalModelKey*`, `TestRankAlias*`, `TestFindAvailableAliasModel*` pass.
- `go build ./...`, `go vet ./pool/` sạch.

## Risk Assessment

- False-positive canonical (model thật kết thúc `-cn`): escape hatch sẵn có — `ModelMappings`
  redirect vẫn reject account ở `accountHasModel:530`; danh sách suffix cứng chỉ 2 locale + date.
- Overhead: rescue chỉ chạy khi exact unavailable (đường lỗi), happy path không probe.

## Next Steps

Phase 02 wire helper vào 3 entry points.
