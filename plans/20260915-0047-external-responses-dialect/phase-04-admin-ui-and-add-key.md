# Phase 04 — Chọn dialect khi add key

**Priority:** P1
**Trạng thái:** Chưa làm
**Ước lượng:** ~3 giờ
**Phụ thuộc:** Phase 02 (field đã có), Phase 03 (adapter đã chạy)

## Context Links

- Spec: [`design.md`](design.md) §5.5
- API add key: `proxy/handler.go:6280` (route) → `proxy/handler.go:10700` (`apiImportExternalProvider`)
- Form: `web/accounts.js:2786-2803` (`modalExternal`), `:2845-2870` (`importExternal`)
- Locale: `web/locales/en.json`, `vi.json`, `zh.json` — key phẳng, namespace `external.*`
  (xem `vi.json:866-875`)

## Overview

Thêm lựa chọn dialect vào form add key và một input path override chỉ hiện khi chọn
Responses. Backend nhận hai field mới, chuẩn hóa giá trị, lưu vào account.

## Key Insights

- **Backend phải chuẩn hóa, không tin client.** Giá trị lạ → `""` (chat). Config là file
  người vận hành sửa tay được, nên tầng ghi cũng phải phòng thủ như tầng đọc đã làm ở
  `externalAPIDialect`.
- **Input path chỉ hiện khi chọn Responses.** Hiện luôn thì người dùng không biết nó là
  gì và sẽ điền bừa; ẩn/hiện theo dialect là tín hiệu đủ.
- **Locale là ba file.** `en.json`, `vi.json`, `zh.json` cùng bộ key. Thiếu một file thì
  giao diện hiện key thô ở ngôn ngữ đó. Kiểm bằng `rg` sau khi thêm.
- **Route và handler đã có sẵn.** Không thêm endpoint mới — chỉ thêm field vào struct
  body (`handler.go:10701-10710`) và vào literal `config.Account` (`:10748-10763`).
- **AgentRouter dùng chung endpoint này.** `apiImportExternalProvider` phục vụ cả
  `authMethod: "agentrouter"` (`:10743`). Field dialect không có nghĩa với AgentRouter —
  bỏ qua khi `authMethod != "external_openai"` thay vì lưu giá trị chết vào account.

## Requirements

**Functional**
- Form add key external có dropdown dialect: Chat Completions (mặc định) / Responses API.
- Khi chọn Responses, hiện thêm input "Responses path (tùy chọn)" với placeholder
  `/v1/responses`.
- Backend nhận `externalApiDialect` và `responsesPath`, chuẩn hóa, lưu.
- Account chọn Chat không lưu hai field này.

**Non-functional**
- Không đổi giao diện của account hiện có (form edit giữ nguyên — YAGNI, thêm sau nếu cần).
- Ba file locale đủ key.

## Architecture

```
web/accounts.js modalExternal
  ├── <select id="externalDialect">  chat | responses
  └── <div id="externalResponsesPathGroup" hidden>  ← hiện khi chọn responses

importExternal()  → POST /auth/external-provider
                      { baseUrl, apiKey, name, test, externalApiDialect, responsesPath }

proxy/handler.go apiImportExternalProvider
  ├── chuẩn hóa dialect: "responses" | ""
  └── config.Account{ ..., ExternalAPIDialect, ResponsesPath }
```

## Related Code Files

**Sửa**
- `proxy/handler.go:10700-10763` — body struct + literal account
- `web/accounts.js:2786-2803` — thêm select + input ẩn
- `web/accounts.js:2845-2870` — gửi field
- `web/locales/en.json`, `web/locales/vi.json`, `web/locales/zh.json` — 6 key mới

**Tạo mới**
- `proxy/external_provider_dialect_test.go` — test handler

## Implementation Steps

### Task 4.1: Backend nhận và chuẩn hóa field

- [ ] **Step 1: Viết test fail**

Tạo `proxy/external_provider_dialect_test.go`.

Test dùng lại `setupResponsesTestHandler(t)` (`proxy/responses_handler_test.go:316`) — nó
đã lo phần khó: trỏ config vào file tạm qua `config.Init`, dựng `accountpool` và gán
`h.pool` + `h.promptCache`. `apiImportExternalProvider` gọi `h.pool.Reload()` nên
`&Handler{}` rỗng sẽ panic — dùng helper, đừng tự dựng Handler.

Hai điều cần biết về handler này:

- Nó gọi `config.AddAccount` (`proxy/handler.go:10779`) → ghi ra config, nên **bắt buộc**
  phải có config tạm, helper lo việc đó.
- Nó spawn `safeGo(fetchAndCacheAccountModels)` (`:10787`) chạy nền. Vì vậy `baseUrl`
  trong test trỏ vào một `httptest` loopback trả `{"data":[]}` — nếu trỏ vào host
  `.invalid`, goroutine nền sẽ thử DNS thật. `safeGo` có recover nên không làm chết test,
  nhưng để nó chạy trên loopback thì test tất định và không phụ thuộc mạng.

```go
package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// dialectTestServer stands in for a gateway. It answers the model-list fetch the
// handler spawns after saving the account, so the background goroutine stays on
// loopback instead of attempting a real lookup.
func dialectTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func importExternalProvider(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/external-provider", strings.NewReader(body))
	h.apiImportExternalProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func dialectOf(t *testing.T, baseURL string) string {
	t.Helper()
	for _, a := range config.GetAccounts() {
		if a.BaseURL == baseURL {
			return a.ExternalAPIDialect
		}
	}
	t.Fatalf("no account stored for baseUrl %q", baseURL)
	return ""
}

// The handler must normalise the dialect rather than trusting the client: the
// config file is hand-editable, so a value that reaches it by any path still has
// to mean something the adapter understands.
func TestImportExternalProviderNormalizesDialect(t *testing.T) {
	cases := []struct {
		name string
		sent string
		want string
	}{
		{"responses", "responses", "responses"},
		{"responses uppercase", "Responses", "responses"},
		{"chat explicit", "chat", ""},
		{"unknown value", "grpc", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := dialectTestServer(t)
			body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"d","externalApiDialect":"` + tc.sent + `"}`
			importExternalProvider(t, body)

			if got := dialectOf(t, srv.URL); got != tc.want {
				t.Fatalf("ExternalAPIDialect = %q, want %q", got, tc.want)
			}
		})
	}
}

// AgentRouter shares this endpoint. The dialect field selects between OpenAI
// dialects, which AgentRouter does not speak, so it must not be persisted there.
func TestImportExternalProviderIgnoresDialectForAgentRouter(t *testing.T) {
	srv := dialectTestServer(t)
	body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"ar","authMethod":"agentrouter","externalApiDialect":"responses"}`
	importExternalProvider(t, body)

	if got := dialectOf(t, srv.URL); got != "" {
		t.Fatalf("AgentRouter account kept dialect %q", got)
	}
}

// responsesPath only means something on the Responses dialect: storing it on a
// chat account would leave a field in the config that reads as if it were in use.
func TestImportExternalProviderStoresResponsesPathOnlyForResponses(t *testing.T) {
	srv := dialectTestServer(t)
	body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"p","externalApiDialect":"responses","responsesPath":"  /codex/responses  "}`
	importExternalProvider(t, body)

	for _, a := range config.GetAccounts() {
		if a.BaseURL != srv.URL {
			continue
		}
		if a.ResponsesPath != "/codex/responses" {
			t.Fatalf("ResponsesPath = %q, want %q", a.ResponsesPath, "/codex/responses")
		}
		return
	}
	t.Fatalf("no account stored for baseUrl %q", srv.URL)
}
```

Nếu `setupResponsesTestHandler` không dùng được vì lý do nào đó, thay bằng cách dựng
Handler mà `proxy/desktop_sota_stream_test.go:27` (`newSOTATestHandler`) đang dùng —
nguyên tắc không đổi: config tạm + `h.pool` có giá trị.

- [ ] **Step 2: Chạy test để xác nhận fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestImportExternalProvider -v
```

Kỳ vọng: FAIL — dialect luôn rỗng vì handler chưa đọc field.

- [ ] **Step 3: Thêm field vào body struct**

Trong `apiImportExternalProvider` (`proxy/handler.go:10701-10710`):

```go
	var body struct {
		BaseURL            string `json:"baseUrl"`
		ApiKey             string `json:"apiKey"`
		Name               string `json:"name"`
		Nickname           string `json:"nickname"`
		AuthMethod         string `json:"authMethod"`
		Weight             int    `json:"weight"`
		ProxyURL           string `json:"proxyURL"`
		ExternalAPIDialect string `json:"externalApiDialect"`
		ResponsesPath      string `json:"responsesPath"`
		Test               bool   `json:"test"`
	}
```

- [ ] **Step 4: Chuẩn hóa và gán vào account**

Ngay trước khối `account := config.Account{...}` (`proxy/handler.go:10748`):

```go
	// Normalize rather than trust the client. externalAPIDialect already treats an
	// unrecognised value as chat, but storing "grpc" would leave a value in the
	// config that reads as if it meant something. AgentRouter shares this endpoint
	// and speaks no OpenAI dialect, so the field is dropped there.
	externalAPIDialectValue := ""
	if authMethod == externalAuthMethod && strings.EqualFold(strings.TrimSpace(body.ExternalAPIDialect), "responses") {
		externalAPIDialectValue = "responses"
	}
	responsesPath := ""
	if externalAPIDialectValue == "responses" {
		responsesPath = strings.TrimSpace(body.ResponsesPath)
	}
```

Thêm vào literal account, sau `ProxyURL`:

```go
		ExternalAPIDialect: externalAPIDialectValue,
		ResponsesPath:      responsesPath,
```

- [ ] **Step 5: Chạy test để xác nhận pass**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestImportExternalProvider -v
```

Kỳ vọng: PASS — 5 subtest của `NormalizesDialect` (test cha) + 2 test còn lại.

### Task 4.2: Dropdown trong form add key

- [ ] **Step 1: Thêm select và input path vào `modalExternal`**

Trong `web/accounts.js`, chèn vào `body.innerHTML` của `modalExternal` (`:2795`, ngay sau
input `externalName`, trước checkbox test):

```js
      '<div class="form-group"><label>' + escapeHtml(t('external.dialectLabel')) + '</label>' +
      '<select id="externalDialect">' +
      '<option value="chat" selected>' + escapeHtml(t('external.dialectChat')) + '</option>' +
      '<option value="responses">' + escapeHtml(t('external.dialectResponses')) + '</option>' +
      '</select>' +
      '<span class="help-block text-xs">' + escapeHtml(t('external.dialectHelp')) + '</span></div>' +
      '<div class="form-group hidden" id="externalResponsesPathGroup"><label>' +
      escapeHtml(t('external.responsesPathLabel')) + ' <span class="muted-text">(' + escapeHtml(t('common.optional') || 'optional') + ')</span></label>' +
      '<input type="text" id="externalResponsesPath" class="font-mono" placeholder="/v1/responses" /></div>' +
```

- [ ] **Step 2: Nối sự kiện ẩn/hiện**

Cuối `modalExternal`, cạnh dòng `$('importExternalBtn').addEventListener(...)` (`:2802`):

```js
    const dialectSel = $('externalDialect');
    const pathGroup = $('externalResponsesPathGroup');
    dialectSel.addEventListener('change', function () {
      pathGroup.classList.toggle('hidden', dialectSel.value !== 'responses');
    });
```

- [ ] **Step 3: Gửi field khi submit**

Trong `importExternal` (`:2845-2855`), thêm trước lời gọi `api(...)`:

```js
    const dialect = ($('externalDialect') || {}).value || 'chat';
    const responsesPath = dialect === 'responses'
      ? (($('externalResponsesPath') || {}).value || '').trim()
      : '';
```

Và sửa payload (`:2855`):

```js
      const res = await api('/auth/external-provider', { method: 'POST', body: JSON.stringify({ baseUrl, apiKey, name, test, externalApiDialect: dialect, responsesPath }) });
```

- [ ] **Step 4: Kiểm bằng mắt trong trình duyệt**

Mở admin UI → Add account → External OpenAI. Kỳ vọng:

1. Dropdown hiện, mặc định "Chat Completions".
2. Chọn "Responses API" → ô path hiện ra.
3. Chọn lại "Chat Completions" → ô path ẩn đi.
4. Thêm một account với Responses + path `/codex/responses`, kiểm trong danh sách account
   (hoặc `data/config.json`) thấy `externalApiDialect: "responses"` và `responsesPath`.

### Task 4.3: Locale

- [ ] **Step 1: Thêm 6 key vào cả ba file**

Thêm cùng bộ key vào `web/locales/en.json`, `vi.json`, `zh.json`, đặt cạnh các key
`external.*` hiện có (`vi.json:866-875`):

**en.json**
```json
  "external.dialectLabel": "API dialect",
  "external.dialectChat": "Chat Completions (default)",
  "external.dialectResponses": "Responses API",
  "external.dialectHelp": "Use Responses only if this gateway speaks it. Most resale gateways serve Chat Completions only.",
  "external.responsesPathLabel": "Responses path",
  "external.responsesPathPlaceholder": "/v1/responses",
```

**vi.json**
```json
  "external.dialectLabel": "Chuẩn API",
  "external.dialectChat": "Chat Completions (mặc định)",
  "external.dialectResponses": "Responses API",
  "external.dialectHelp": "Chỉ chọn Responses nếu gateway này hỗ trợ. Phần lớn gateway resale chỉ có Chat Completions.",
  "external.responsesPathLabel": "Đường dẫn Responses",
  "external.responsesPathPlaceholder": "/v1/responses",
```

**zh.json**
```json
  "external.dialectLabel": "API 协议",
  "external.dialectChat": "Chat Completions（默认）",
  "external.dialectResponses": "Responses API",
  "external.dialectHelp": "仅当该网关支持时才选择 Responses。大多数转售网关仅提供 Chat Completions。",
  "external.responsesPathLabel": "Responses 路径",
  "external.responsesPathPlaceholder": "/v1/responses",
```

- [ ] **Step 2: Xác nhận JSON hợp lệ và đủ key**

```bash
cd /Users/van/Tools/OmniProxy && for f in en vi zh; do echo "--- $f"; python3 -c "import json,sys; json.load(open('web/locales/$f.json'))" && echo ok; rg -c 'external\.dialectLabel' web/locales/$f.json; done
```

Kỳ vọng: mỗi file in `ok` và `1`.

- [ ] **Step 3: Commit**

```bash
cd /Users/van/Tools/OmniProxy && git add proxy/handler.go proxy/external_provider_dialect_test.go web/accounts.js web/locales/en.json web/locales/vi.json web/locales/zh.json && git commit -m "feat(web): expose Responses dialect selection when adding an external key"
```

## Todo List

- [ ] 4.1 Backend nhận + chuẩn hóa field (có test)
- [ ] 4.2 Dropdown + input path trong form add key (kiểm tay)
- [ ] 4.3 Locale 3 file + commit

## Success Criteria

- Thêm account qua UI với Responses lưu đúng `externalApiDialect` + `responsesPath`.
- Thêm account với Chat không ghi hai field đó.
- AgentRouter không bao giờ lưu dialect.
- Ba file locale đủ key, JSON hợp lệ.
- Giao diện account hiện có không đổi.

## Risk Assessment

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Quên một file locale → hiện key thô | Vừa | Task 4.3 Step 2 kiểm bằng script |
| Field rò sang AgentRouter | Thấp | Có test riêng |
| `hidden` class không tồn tại trong CSS | Thấp | Đã dùng ở `apiCli.js:1680`; kiểm tay ở Step 4 |

## Security Considerations

- Handler không log `apiKey`; không đổi hành vi đó.
- Không có endpoint mới, không đổi kiểm tra quyền của `/auth/external-provider`.
- `responsesPath` là dữ liệu người vận hành nhập và được ghép vào URL outbound. URL guard
  hiện có (`proxy/urlguard.go`) vẫn chạy trên mọi request outbound — không bỏ qua nó.
- Không nhận HTML từ người dùng vào DOM: mọi chuỗi qua `escapeHtml` / `escapeAttr`.

## Next Steps

Phase 05 chạy hồi quy toàn bộ và đo thật trên gateway.
