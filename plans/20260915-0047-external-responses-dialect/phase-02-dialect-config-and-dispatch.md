# Phase 02 — Config field, dialect helper, rẽ nhánh dispatch

**Priority:** P0
**Trạng thái:** Chưa làm
**Ước lượng:** ~2 giờ
**Phụ thuộc:** Phase 01 xong (build sạch)

## Context Links

- Spec: [`design.md`](design.md) §5.1, §5.2
- Pattern path override: `config/config.go` (`ChatPath`) và `proxy/external_openai.go` (`externalChatPath`)
- Điểm rẽ nhánh: `proxy/external_openai.go` (`dispatchChat`)

Số dòng không ghi ở đây vì repo đang có 7 file WIP chưa commit làm chúng lệch. Tìm bằng
grep theo tên hàm.

## Overview

Thêm hai field vào `config.Account`: `ExternalAPIDialect` và `ResponsesPath`. Thêm helper
đọc dialect, helper đọc path, và nhánh rẽ trong `dispatchChat`. **Không có setter** — lý do
ở deviation #4 của [`plan.md`](plan.md).

Phase này **chưa** có adapter Responses — nhánh rẽ gọi một hàm sẽ được thêm ở Phase 03.
Để build xanh trong lúc chờ, Task 2.3 tạo hàm đó dưới dạng stub trả lỗi rõ ràng, và
Phase 03 thay bằng implementation thật. Cách này giữ mỗi phase tự build được.

## Key Insights

- Không tái dùng `ChatPath` cho Responses. `ChatPath` là *path*, không phải *dialect*:
  trỏ `/v1/responses` vào đó vẫn gửi body chat-completions và gateway trả 400. Hai khái
  niệm khác nhau, hai field khác nhau.
- `externalChatPath` (`proxy/external_openai.go`) là mẫu chính xác cho helper path: trim,
  thêm `/` đầu, fallback về hằng mặc định, nil-safe.
- Field chỉ có nghĩa với `AuthMethod == "external_openai"`. Account AgentRouter dùng
  chung adapter external nhưng đi qua `CallExternalAgentRouter` — set field trên đó bị bỏ
  qua, không báo lỗi.
- Nhánh rẽ đặt **trong** khối `isExternalAccount` ở `dispatchChat`, **sau** các nhánh
  `isCodexAccount` / `isAntigravityAccount` / `isAgentRouterAccount`. Thứ tự quan trọng:
  đặt trước sẽ nuốt mất các loại account đó.

## Requirements

**Functional**
- `ExternalAPIDialect`: rỗng hoặc `"chat"` → chat completions. `"responses"` → Responses.
  Giá trị khác → coi như `"chat"` (không phá account cũ khi config bị sửa tay).
- `ResponsesPath`: override path Responses, rỗng → `"/v1/responses"`.
- Field chỉ được ghi lúc add key (Phase 04) và chỉ tồn tại nhờ tag JSON của nó. Không có
  setter, không có đường sửa sau — xem deviation #4.

**Non-functional**
- So khớp dialect không phân biệt hoa thường, có trim khoảng trắng.
- Nil account không panic.

## Architecture

```
config.Account
  ├── ExternalAPIDialect string  (json: externalApiDialect)
  └── ResponsesPath       string  (json: responsesPath)

proxy/external_openai_responses.go
  ├── defaultExternalResponsesPath = "/v1/responses"
  ├── externalResponsesPath(account) string
  ├── externalAPIDialect(account) string   → "chat" | "responses"
  └── CallExternalOpenAIResponses(...)     → stub ở phase này

proxy/external_openai.go dispatchChat
  └── isExternalAccount → rẽ theo externalAPIDialect
```

## Related Code Files

**Sửa**
- `config/config.go` — 2 field (cạnh `ChatPath`). **Không thêm setter.**
- `proxy/external_openai.go` — nhánh rẽ trong `dispatchChat`

**Tạo mới**
- `proxy/external_openai_responses.go`
- `proxy/external_openai_responses_test.go`
- `config/account_dialect_test.go`

## Implementation Steps

### Task 2.1: Field trong config

**Không có setter.** Design doc §5.1 yêu cầu `SetAccountExternalAPIDialect`, nhưng nó đã bị
bỏ sau khi kiểm chứng — xem deviation #4 ở [`plan.md`](plan.md). Tóm tắt: `externalApiDialect`
là field do người dùng nhập, giống mọi field khác của `apiUpdateAccount`, và hàm đó persist
bằng cách **thay toàn bộ** record (`config.UpdateAccount` → `cfg.Accounts[i] = account`,
`config/config.go:1345`), nên một setter gọi giữa chừng sẽ bị ghi đè. Field được ghi trực
tiếp lên account literal lúc add key (Task 4.1).

- [ ] **Step 1: Viết test fail**

Tạo `config/account_dialect_test.go`. Bám đúng idiom đã có ở
`config/account_allowed_models_test.go:36-74`: seed file config bằng JSON, `Init`, đọc
lại bằng `GetAccounts`, ghi lại, rồi soi key trên đĩa. Không tạo helper cô lập mới —
`filepath.Join(t.TempDir(), "config.json")` là cách repo đang dùng.

```go
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The admin UI posts externalApiDialect / responsesPath and the add-key handler
// writes them straight onto the account literal, so the JSON tag names are the
// entire contract between the two. A renamed tag would silently drop the
// operator's choice, which is why this test asserts on the on-disk key rather
// than only on the struct field.
func TestExternalAPIDialectSurvivesLoadAndSave(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[{"id":"dialect-a","enabled":true,` +
		`"externalApiDialect":"responses","responsesPath":"/codex/responses"}]}`)
	if err := os.WriteFile(cfgFile, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}

	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if got := accounts[0].ExternalAPIDialect; got != "responses" {
		t.Fatalf("loaded ExternalAPIDialect = %q, want %q", got, "responses")
	}
	if got := accounts[0].ResponsesPath; got != "/codex/responses" {
		t.Fatalf("loaded ResponsesPath = %q, want %q", got, "/codex/responses")
	}
	if err := UpdateAccountPreservingCredentials("dialect-a", accounts[0]); err != nil {
		t.Fatalf("save account: %v", err)
	}

	onDisk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(onDisk, &persisted); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	if len(persisted.Accounts) != 1 {
		t.Fatalf("persisted accounts = %d, want 1", len(persisted.Accounts))
	}
	if got := persisted.Accounts[0]["externalApiDialect"]; got != "responses" {
		t.Fatalf("persisted externalApiDialect = %v, want %q", got, "responses")
	}
	if got := persisted.Accounts[0]["responsesPath"]; got != "/codex/responses" {
		t.Fatalf("persisted responsesPath = %v, want %q", got, "/codex/responses")
	}
}

// Every external account that existed before this feature has neither key. The
// omitempty tags are what keep those stored records byte-identical after an
// unrelated edit, so a stray zero-value write must fail here rather than quietly
// rewrite the config of accounts the operator never touched.
func TestChatDialectAccountsOmitBothKeys(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"accounts":[{"id":"chat-a","enabled":true}]}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if err := UpdateAccountPreservingCredentials("chat-a", accounts[0]); err != nil {
		t.Fatalf("save account: %v", err)
	}

	onDisk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(onDisk, &persisted); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	for _, key := range []string{"externalApiDialect", "responsesPath"} {
		if _, present := persisted.Accounts[0][key]; present {
			t.Fatalf("%s must be omitted for a chat-dialect account", key)
		}
	}
}
```

- [ ] **Step 2: Chạy test để xác nhận fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./config/ -run 'TestExternalAPIDialect|TestChatDialectAccounts' -v
```

Kỳ vọng: FAIL khi build — `accounts[0].ExternalAPIDialect undefined` và
`accounts[0].ResponsesPath undefined` (field chưa tồn tại).

- [ ] **Step 3: Thêm field vào `config.Account`**

Chèn ngay sau `ExternalHeaderProfile` (`config/config.go:153`):

```go
	// ExternalAPIDialect selects the outbound OpenAI dialect for this external
	// account. Empty and "chat" both mean Chat Completions
	// ({BaseURL}/v1/chat/completions), which is what every external account used
	// before this field existed. "responses" routes the same payload through the
	// Responses API shape instead, which is required by gateways whose backend
	// only speaks Responses — for example a reseller of ChatGPT Codex
	// subscription capacity.
	//
	// It is opt-in per account because the external pool is heterogeneous: most
	// resale gateways serve chat only, so a global default would break accounts
	// the operator never touched.
	ExternalAPIDialect string `json:"externalApiDialect,omitempty"`

	// ResponsesPath overrides the upstream Responses path for this account.
	// Empty means the OpenAI default "/v1/responses".
	//
	// It exists for the same reason ChatPath does: a gateway whose Responses
	// route sits behind a prefix (Codex-style resellers commonly serve
	// ".../codex/responses" rather than "/v1/responses") would otherwise be
	// unreachable even though the dialect is correct.
	ResponsesPath string `json:"responsesPath,omitempty"`
```

- [ ] **Step 4: Chạy test để xác nhận pass**

```bash
cd /Users/van/Tools/OmniProxy && go test ./config/ -v 2>&1 | tail -20
```

Kỳ vọng: PASS toàn bộ package `config`.

Ghi chú: nếu `go test ./config/` báo lỗi biên dịch ở file test khác thì đó là do
field chưa được thêm đúng chỗ — đọc lại thông báo lỗi, đừng sửa test cũ.


### Task 2.2: Helper dialect và path

- [ ] **Step 1: Viết test fail**

Tạo `proxy/external_openai_responses_test.go`:

```go
package proxy

import (
	"testing"

	"omniproxy/config"
)

func TestExternalAPIDialect(t *testing.T) {
	cases := []struct {
		name    string
		account *config.Account
		want    string
	}{
		{"nil account", nil, "chat"},
		{"empty", &config.Account{}, "chat"},
		{"chat", &config.Account{ExternalAPIDialect: "chat"}, "chat"},
		{"responses", &config.Account{ExternalAPIDialect: "responses"}, "responses"},
		{"responses uppercase", &config.Account{ExternalAPIDialect: "Responses"}, "responses"},
		{"responses padded", &config.Account{ExternalAPIDialect: "  responses  "}, "responses"},
		{"unknown value falls back to chat", &config.Account{ExternalAPIDialect: "grpc"}, "chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalAPIDialect(tc.account); got != tc.want {
				t.Fatalf("externalAPIDialect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExternalResponsesPath(t *testing.T) {
	cases := []struct {
		name    string
		account *config.Account
		want    string
	}{
		{"nil account", nil, "/v1/responses"},
		{"empty override", &config.Account{}, "/v1/responses"},
		{"override without slash", &config.Account{ResponsesPath: "v1/responses"}, "/v1/responses"},
		{"codex-style prefix", &config.Account{ResponsesPath: "/codex/responses"}, "/codex/responses"},
		{"whitespace only", &config.Account{ResponsesPath: "   "}, "/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalResponsesPath(tc.account); got != tc.want {
				t.Fatalf("externalResponsesPath = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Chạy test để xác nhận fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'TestExternalAPIDialect|TestExternalResponsesPath' -v
```

Kỳ vọng: FAIL — `undefined: externalAPIDialect`, `undefined: externalResponsesPath`.

- [ ] **Step 3: Tạo `proxy/external_openai_responses.go`**

```go
// Package proxy — external OpenAI-compatible provider adapter, Responses dialect.
//
// Accounts with AuthMethod == "external_openai" normally forward chat-completion
// requests to {BaseURL}/v1/chat/completions. An account may instead select the
// Responses API dialect (config.Account.ExternalAPIDialect == "responses"), which
// is required by gateways whose backend only speaks Responses — a reseller of
// ChatGPT Codex subscription capacity, for example. Such a gateway has to
// translate chat completions into Responses internally, and that translation
// loses the reasoning items the model would otherwise carry between turns.
//
// The wire translation is shared with the Codex adapter and lives in
// responses_upstream.go; this file owns only the account-level selection and the
// transport, which is the same one the chat dialect uses.
package proxy

import (
	"strings"

	"omniproxy/config"
)

// defaultExternalResponsesPath is the OpenAI Responses path used unless the
// account overrides it, mirroring defaultExternalChatPath for chat.
const defaultExternalResponsesPath = "/v1/responses"

// externalAPIDialect reports which outbound OpenAI dialect an account uses:
// "responses" when explicitly selected, otherwise "chat". Unrecognised values
// fall back to chat rather than erroring, so a hand-edited config can never take
// an account offline over a typo.
func externalAPIDialect(account *config.Account) string {
	if account == nil {
		return "chat"
	}
	if strings.EqualFold(strings.TrimSpace(account.ExternalAPIDialect), "responses") {
		return "responses"
	}
	return "chat"
}

// externalResponsesPath returns the upstream Responses path for an account: the
// account-level override when set, otherwise the OpenAI default.
func externalResponsesPath(account *config.Account) string {
	if account == nil {
		return defaultExternalResponsesPath
	}
	if p := strings.TrimSpace(account.ResponsesPath); p != "" {
		return "/" + strings.TrimLeft(p, "/")
	}
	return defaultExternalResponsesPath
}
```

- [ ] **Step 4: Chạy test để xác nhận pass**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'TestExternalAPIDialect|TestExternalResponsesPath' -v
```

Kỳ vọng: PASS cả 12 subtest.

### Task 2.3: Rẽ nhánh trong `dispatchChat`

- [ ] **Step 1: Thêm stub adapter**

Phase 03 sẽ thay stub này bằng implementation thật. Stub tồn tại để mỗi phase build được
và để test rẽ nhánh chạy được ngay.

Thêm vào cuối `proxy/external_openai_responses.go`:

```go
// CallExternalOpenAIResponses forwards a KiroPayload to an external
// OpenAI-compatible provider using the Responses API dialect.
//
// Implemented in phase 03.
func CallExternalOpenAIResponses(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return fmt.Errorf("external responses dialect not implemented yet for %s", accountLabel(account))
}
```

Thêm `"context"` và `"fmt"` vào import block của file.

- [ ] **Step 2: Viết test fail cho rẽ nhánh**

Thêm vào `proxy/external_openai_responses_test.go`:

```go
// The stub returns a distinguishable error, so this test proves the branch routes
// to the Responses adapter rather than the chat one without needing a live server.
func TestDispatchChatRoutesResponsesDialect(t *testing.T) {
	account := &config.Account{
		ID:                 "dialect-route",
		AuthMethod:         "external_openai",
		BaseURL:            "https://example.invalid",
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
	err := dispatchChat(context.Background(), account, &KiroPayload{}, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected the responses stub to return an error")
	}
	if !strings.Contains(err.Error(), "responses dialect") {
		t.Fatalf("dispatchChat routed to the wrong adapter: %v", err)
	}
}
```

Thêm `"context"` và `"strings"` vào import block của file test.

- [ ] **Step 3: Chạy test để xác nhận fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestDispatchChatRoutesResponsesDialect -v
```

Kỳ vọng: FAIL — lỗi trả về là của chat adapter (`has no baseUrl` hoặc lỗi build request),
không chứa `"responses dialect"`.

- [ ] **Step 4: Sửa `dispatchChat`**

Trong `proxy/external_openai.go:1798-1800`, thay:

```go
	if isExternalAccount(account) {
		// The external pool is heterogeneous: most resale gateways serve chat
		// completions only, so the Responses dialect is opt-in per account
		// rather than a default.
		if externalAPIDialect(account) == "responses" {
			return CallExternalOpenAIResponses(ctx, account, payload, callback)
		}
		return CallExternalOpenAI(ctx, account, payload, callback)
	}
```

- [ ] **Step 5: Chạy test để xác nhận pass**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'TestDispatchChatRoutesResponsesDialect|TestExternal' -v 2>&1 | tail -20
```

Kỳ vọng: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/van/Tools/OmniProxy && git add config/config.go config/account_dialect_test.go proxy/external_openai.go proxy/external_openai_responses.go proxy/external_openai_responses_test.go && git commit -m "feat(external): add per-account Responses API dialect selection"
```

## Todo List

- [ ] 2.1 Field `ExternalAPIDialect` + `ResponsesPath` (có test). **Không setter.**
- [ ] 2.2 `externalAPIDialect()` + `externalResponsesPath()` (có test)
- [ ] 2.3 Stub adapter + rẽ nhánh `dispatchChat` (có test)

## Success Criteria

- `go test ./config/ ./proxy/` xanh.
- `externalAPIDialect` trả `"chat"` cho nil, rỗng, giá trị lạ; `"responses"` cho biến thể
  hoa thường và có khoảng trắng.
- Account `external_openai` không set field chạy y như trước (đường chat).
- `dispatchChat` với field `responses` đi vào adapter Responses.

## Risk Assessment

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Đặt nhánh rẽ sai thứ tự, nuốt account Codex/AgentRouter | Vừa | Nhánh nằm trong `isExternalAccount`, sau 3 nhánh kia; test route |
| Config cũ có giá trị lạ ở field mới | Thấp | Fallback về `"chat"` thay vì lỗi |
| Stub lọt vào production nếu Phase 03 bị bỏ | Vừa | Stub trả lỗi rõ ràng, không im lặng; Phase 05 kiểm `rg` không còn stub |

## Security Considerations

Không có credential mới. Không log `AccessToken`. Field mới chỉ ảnh hưởng routing outbound,
không mở bề mặt nhận request.

## Next Steps

Phase 03 thay stub bằng implementation thật. Phase 04 thêm field vào form add key.
