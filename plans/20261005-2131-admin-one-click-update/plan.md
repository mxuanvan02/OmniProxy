# Plan: Hoàn thiện `/admin/` với luồng cập nhật 1 nút + release v0.6.1

## Context Links
- `scripts/update.sh` — script cập nhật đã có sẵn (tải → verify sha256 → backup → swap → restart → health-check → rollback)
- `.github/workflows/release.yml` — build 5 nền tảng khi push tag `v*`
- `web/app.js:788-851` — `checkUpdate()` + `showUpdateModal()` đã có sẵn
- `proxy/handler.go:6256` — `handleAdminAPI` dispatch
- `~/Library/LaunchAgents/com.van.omniproxy.plist` — service trên máy này

## Overview
- **Mục tiêu:** từ UI `/admin/`, bấm 1 nút là cập nhật xong, thấy tiến độ từng bước.
- **Phạm vi:** backend endpoint + nút UI + sửa `update.sh` cho khớp layout máy này + cắt release v0.6.1.
- **Không đụng:** `/admin-next` (đã bỏ theo chỉ đạo).
- **Trạng thái:** plan, chưa viết code.

## Key Insights (từ khảo sát)

### 1. Phần "phát hiện bản mới" ĐÃ CÓ — không cần làm backend
`web/app.js:794` đã fetch thẳng `raw.githubusercontent.com/.../version.json`, so version, và đã có `showUpdateModal()`. **Kế hoạch ban đầu định thêm `latestVersion` vào `/admin/api/status` là thừa** — bỏ hẳn.

⇒ Khoảng trống thật chỉ là: **modal hiện chỉ có nút "Go to download" (mở GitHub), chưa có nút thực thi cập nhật.**

### 2. ⚠️ CHẶN CỨNG: `update.sh` KHÔNG khớp máy này
Script giả định layout `~/.omniproxy-user/bin/{omniproxy,web}` + systemd. Thực tế máy này:
- `~/.omniproxy-user` **không tồn tại**.
- Binary chạy tại `~/Tools/OmniProxy/omniproxy` (ngay trong git working tree, không có `bin/`).
- Restart bằng **launchd** (`com.van.omniproxy`), không phải systemd.

Chạy `update.sh` nguyên bản trên máy này sẽ tạo `~/Tools/OmniProxy/bin/omniproxy` — **launchd vẫn chạy binary cũ**, update im lặng không có tác dụng.

⇒ Phải thêm **layout mode** vào `update.sh` (giữ mặc định cũ cho máy Linux/systemd), không viết lại bằng Go — vì "máy khác" của anh có thể đúng layout chuẩn.

### 3. Release tarball KHÔNG chứa `scripts/`
`release.yml:63` chỉ copy `web` + binary + `version.json`. Nếu endpoint dựa vào script thì phải ship `scripts/` theo, hoặc endpoint tự biết đường dẫn script của bản đang chạy.

### 4. Release notes lấy từ CHANGELOG.md
`release.yml:104` trích mục `## [X.Y.Z]` trong `CHANGELOG.md`. Hiện `[Unreleased]` đang có sẵn nội dung của cả 3 fix → cắt release = đổi `[Unreleased]` → `[0.6.1]`.

## Requirements
**Chức năng**
- Nút "Cập nhật ngay" trong modal → gọi backend → chạy cập nhật → hiện log realtime → báo kết quả.
- Cập nhật thành công: proxy tự restart, UI tự reload, version mới hiển thị đúng.
- Cập nhật thất bại: rollback tự động (script đã lo), UI báo lỗi + hướng dẫn khôi phục tay.
- Vẫn giữ nút "tải thủ công" làm phương án phụ.

**Phi chức năng**
- Chỉ 1 tiến trình cập nhật tại một thời điểm (chống bấm 2 lần).
- Không nhận tham số nào từ client → không có bề mặt injection.
- SSE stream không bị proxy/tường lửa buffer.

## Architecture
```
UI modal ──GET /admin/api/update?token=…──► Go handler
                                              │ exec.CommandContext("bash", <script cố định>)
                                              ▼
                                        update.sh (layout mode)
                                    tải → verify sha256 → backup
                                        → swap → restart → health
                                              │ stdout/stderr
                                              ▼
                                    SSE event: log / success / error
                                              ▼
                              UI: log realtime → poll /v1/models → reload
```
- **GET chứ không POST:** `EventSource` của trình duyệt chỉ hỗ trợ GET; endpoint SSE sẵn có `/admin/api/logs/stream` cũng nhận token qua query param — giữ đồng nhất. Auth dùng chung tầng `adminSessions.valid()`.
- **Restart cắt SSE giữa chừng là bình thường** — UI phải coi `onerror` là tín hiệu "đang restart", không phải lỗi.

## Related Code Files
**Tạo mới**
- `proxy/admin_update.go` — handler `apiTriggerUpdate`
- `proxy/admin_update_test.go` — test auth + đường dẫn script + chống chạy song song

**Sửa**
- `proxy/handler.go` — thêm route vào `handleAdminAPI` (sau khối auth, ~line 6285)
- `scripts/update.sh` — thêm layout mode (`OMNIPROXY_LAYOUT=repo|prefix`)
- `web/app.js` — thêm nút "Cập nhật ngay" trong `showUpdateModal` + hàm `runUpdate()` + modal tiến độ
- `web/styles.css` — style modal tiến độ
- `web/locales/*` — key i18n mới (EN/VI/ZH) theo pattern `update.*` sẵn có
- `version.json` — bump 0.6.1
- `CHANGELOG.md` — `[Unreleased]` → `[0.6.1]`
- `.github/workflows/release.yml` — ship `scripts/` vào tarball

**Không đụng**
- `/admin-next/*`, `admin_next_route_test.go`

## Implementation Steps

### Bước 1 — `update.sh` hỗ trợ layout repo
Thêm biến `LAYOUT` (`prefix` mặc định = hành vi cũ, `repo` = máy này):
- `repo`: `HOME_DIR` = thư mục chứa script, binary tại `${HOME_DIR}/omniproxy`, web tại `${HOME_DIR}/web` (không có `bin/`).
- `repo`: restart mặc định = `launchctl kickstart -k gui/$(id -u)/com.van.omniproxy` nếu `$OMNIPROXY_RESTART_CMD` trống.
- Giữ nguyên toàn bộ logic verify/backup/rollback — chỉ đổi cách suy ra đường dẫn.
- Thêm `--layout` vào help.

### Bước 2 — Backend `GET /admin/api/update`
`proxy/admin_update.go`:
- Xác định script: ưu tiên `scripts/update.sh` cạnh binary đang chạy; nếu không có thì `OMNIPROXY_UPDATE_SCRIPT`. **Đường dẫn tuyệt đối, không nhận từ request.**
- `sync.Mutex` (hoặc `atomic.CompareAndSwap`) — đang chạy thì trả `409` kèm trạng thái hiện tại.
- `exec.CommandContext` với `bash <script> --layout repo`; gom stdout+stderr qua pipe, đẩy từng dòng thành `event: log`.
- Kết thúc: exit 0 → `event: success`; khác → `event: error` kèm dòng cuối của stderr.
- Timeout tổng 5 phút; giữ `event: ping` mỗi 15s để không bị cắt kết nối.
- Route trong `handleAdminAPI`, **sau** khối kiểm tra `adminSessions.valid()`.

### Bước 3 — UI nút "Cập nhật ngay"
`web/app.js`:
- Trong `showUpdateModal`: thêm nút primary "Cập nhật ngay" gọi `runUpdate(version)`; giữ nút tải thủ công thành secondary.
- `runUpdate()`: mở modal tiến độ (textarea readonly + dòng trạng thái + nút Đóng disabled), `new EventSource('/admin/api/update?token=…')`.
  - `log` → append, auto-scroll.
  - `success` → đóng ES, chuyển sang "đang chờ proxy khởi động lại…", poll `/v1/models` mỗi 2s (2xx hoặc 4xx = đã lên); lên rồi thì reload trang; quá 60s thì báo lỗi + hướng dẫn restart tay.
  - `error` → hiện thông báo lỗi, mở nút Đóng.
  - `onerror` (mất kết nối) → coi như restart đang diễn ra, chuyển sang nhánh poll health ở trên. **Không** báo lỗi.
- i18n: thêm key vào `web/locales/*` cho mọi chuỗi mới (EN/VI/ZH).

### Bước 4 — Ship `scripts/` trong tarball
`release.yml`: thêm `cp -r scripts "dist/${name}/scripts"` cạnh dòng copy `web` — để endpoint có script chạy trên máy không clone repo.

### Bước 5 — Cắt release v0.6.1
1. `CHANGELOG.md`: đổi `## [Unreleased]` → `## [0.6.1] — 2026-10-05`.
2. `version.json`: `0.6.0` → `0.6.1`.
3. `git commit -m "chore(release): 0.6.1"`.
4. `git tag v0.6.1 && git push origin main v0.6.1`.
5. Workflow tự build + publish; verify release có đủ 5 `.tar.gz` + 5 `.sha256`.

### Bước 6 — Test end-to-end
Trên máy này: hạ `version.json` xuống bản cũ hơn → mở `/admin/` → toast bản mới → bấm "Cập nhật ngay" → log chảy → proxy restart → UI tự reload → version đúng. Test thêm nhánh lỗi (script fail) để xác nhận rollback + UI báo lỗi.

## Todo List
- [ ] `update.sh`: thêm layout `repo` + restart qua launchctl, cập nhật help
- [ ] `proxy/admin_update.go`: handler SSE + mutex + timeout + ping
- [ ] `proxy/handler.go`: đăng ký route sau khối auth
- [ ] `proxy/admin_update_test.go`: test 401 khi thiếu token, 409 khi đang chạy, đường dẫn script cố định
- [ ] `web/app.js`: nút + `runUpdate()` + modal tiến độ + nhánh poll health
- [ ] `web/styles.css`: style modal tiến độ
- [ ] `web/locales/*`: key i18n EN/VI/ZH
- [ ] `release.yml`: ship `scripts/`
- [ ] `CHANGELOG.md` + `version.json`: bump 0.6.1
- [ ] Tag + push + verify release
- [ ] Test e2e cả nhánh thành công lẫn rollback

## Success Criteria
- Từ `/admin/`, bấm 1 nút → proxy lên bản mới, UI tự reload, không cần SSH.
- Log từng bước hiện realtime trên UI.
- Script fail → rollback tự động, proxy vẫn sống, UI báo lỗi rõ.
- Bấm 2 lần đồng thời → lần 2 nhận 409, không clobber binary.
- Release `v0.6.1` publish đủ 5 nền tảng + checksum, workflow xanh.
- `go test ./...` xanh; `go vet` sạch.

## Risk Assessment
| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| `update.sh` layout sai → update im lặng vô hiệu | **Cao** | Layout mode + verify `version.json` sau update; test e2e bắt buộc |
| Binary bị thay giữa lúc đang chạy → hỏng | Trung | `update.sh` dùng rename nguyên tử; health-check + rollback đã có |
| Rollback cũng fail → proxy chết hẳn | Trung | UI hướng dẫn restart tay; backup nằm ở `.rollback/` |
| 2 request update chạy song song | Trung | Mutex trong handler + trả 409 |
| SSE bị buffer, UI không thấy log | Thấp | `Cache-Control: no-cache`, `X-Accel-Buffering: no`, `event: ping` định kỳ |
| Cập nhật làm rớt chính session đang thao tác | Thấp | Chờ health rồi mới reload; người dùng chủ động bấm nút |

## Security Considerations
- **Không nhận input từ client.** Endpoint không có tham số; đường dẫn script là hằng số tuyệt đối, không ghép từ request.
- **Auth:** dùng chung `adminSessions.valid()`; `/admin/api/update` nằm sau khối kiểm tra token trong `handleAdminAPI`.
- **Không log/echo token** vào SSE payload.
- **Checksum bắt buộc:** `update.sh` đã từ chối cài nếu thiếu `.sha256` — giữ nguyên, không nới.
- **Script phải thuộc repo:** cân nhắc kiểm tra owner/perms trước khi exec (chống ghi đè bởi tiến trình khác cùng máy). Ghi nhận là cải tiến, không chặn bước 1.
- **Không thêm quyền mới** cho tiến trình proxy ngoài việc nó vốn đã chạy cùng user.

## Next Steps
- Chờ duyệt plan → làm bước 1-2 → test tay → bước 3 → bước 4-5 (release) → e2e.

## Open Questions
1. Các "máy khác" của anh dùng layout `~/.omniproxy-user` + systemd, hay cũng clone repo như máy này? Nếu **tất cả** đều là repo-layout thì có thể đơn giản hoá (bỏ nhánh prefix) — nhưng em đề xuất giữ cả hai để không phá bản Linux.
2. Cập nhật binary bằng tarball release (script hiện tại) hay `git pull && go build` tại chỗ? Tarball ổn hơn: đã có checksum, không cần toolchain Go trên máy đích.
