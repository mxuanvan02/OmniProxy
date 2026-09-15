# Phase 01 — Tách helper Responses dùng chung

**Priority:** P0 — làm đầu tiên
**Trạng thái:** Chưa làm
**Ước lượng:** ~4 giờ

## Context Links

- Spec: [`design.md`](design.md) §5.3, §8
- Code nguồn: `proxy/external_codex.go:496-1128`
- Test hiện có sẽ chạy lại: `proxy/external_codex_test.go`, `proxy/codex_cache_test.go`

## Overview

`external_codex.go` đang chứa phần dịch `KiroPayload` → Responses request body và
parse Responses SSE/JSON. Phần này **gần như đã generic**; chỉ 3 chỗ phụ thuộc Codex.
Phase này tách nó ra `proxy/responses_upstream.go` và tham số hóa 3 chỗ đó, **không
đổi hành vi**.

Đây là phase rủi ro cao nhất: đụng vào code mà mọi account Codex đang chạy phụ thuộc.
Cổng chặn là một test golden chụp lại body hiện tại **trước khi** refactor.

## Key Insights

Đọc kỹ 3 chỗ Codex-specific trong `kiroPayloadToCodexResponsesRequest`:

1. **Dòng 509-511** — model mặc định cứng `"gpt-5.6-sol"` khi payload không có model.
2. **Dòng 643-651** — `temperature` / `top_p` **không** được gửi, kèm comment giải
   thích backend ChatGPT từ chối chúng với HTTP 400 trên GPT-5.x reasoning. Lưu ý:
   code hiện tại **bỏ qua** hai tham số này hoàn toàn, không phải "strip" — nên option
   phải là *bật gửi* (`ForwardSamplingParams`), không phải *tắt gửi*.
3. **Dòng 633** — `codexToolDescription(name, ...)` nhồi guidance vòng đời Task/Stop
   của Codex CLI vào mọi tool description.

Ngoài 3 chỗ đó, toàn bộ hàm là dịch thuần: history → `input` items, priming pair →
`instructions`, `ToolResults` → `function_call_output`, `ToolUses` → `function_call`,
tools → shape phẳng, `ToolChoice` → `responsesToolChoice`.

`codexToolChoice` (`:658`) và `codexMessageContent` (`:710`) **không** dùng ở đâu
ngoài `external_codex.go` — kiểm bằng `rg`. An toàn để đổi tên.

`newCodexCoalescer` (`:1129`) được dùng ở `proxy/handler.go:4276`, `proxy/handler.go:5531`,
`proxy/responses_handler.go:649`. **Không đụng vào nó** — nó là stream smoother, không
phải thành phần dialect.

## Requirements

**Functional**
- Hàm dựng body và các hàm parse chuyển sang `proxy/responses_upstream.go`, tên bỏ tiền tố `codex`.
- 3 hành vi Codex-specific trở thành field của một struct options.
- `CallExternalCodex` gọi helper mới với options Codex; hành vi **byte-identical**.

**Non-functional**
- Không file nào vượt 200 dòng vì thay đổi này (file mới ~400 dòng là chấp nhận được
  vì nó gom 1 trách nhiệm; nếu vượt nhiều thì tách parse ra file riêng).
- `go vet ./...` sạch.

## Architecture

```
proxy/responses_upstream.go   (mới)
  ├── type responsesDialectOptions
  ├── kiroPayloadToResponsesRequest(payload, account, opts)
  ├── responsesToolChoice / responsesMessageContent
  ├── parseResponsesSSE / processResponsesSSELine / parseResponsesJSON
  └── bufferedReadCloser

proxy/external_codex.go       (sửa)
  ├── codexResponsesOptions()  → DefaultModel + codexToolDescription
  ├── kiroPayloadToCodexResponsesRequest(...)  → wrapper mỏng, giữ tên cũ
  └── codexToolDescription     → ở lại (Codex-specific)
```

Wrapper `kiroPayloadToCodexResponsesRequest` giữ nguyên tên vì `external_codex_test.go`
gọi nó ở 4 chỗ (`:255`, `:308`, `:323`, `:438`) — giữ wrapper thì không phải sửa test cũ.

## Related Code Files

**Tạo mới**
- `proxy/responses_upstream.go`
- `proxy/responses_upstream_test.go`
- `proxy/testdata/codex_responses_body.golden.json`

**Sửa**
- `proxy/external_codex.go` — xóa code đã chuyển, thêm wrapper + options
- `proxy/codex_cache_test.go:83` — `parseCodexResponsesJSON` → `parseResponsesJSON`
- `proxy/codex_cache_test.go:8,58` — `processCodexSSELine` → `processResponsesSSELine`, `codexToolAccum` → `responsesToolAccum`

## Implementation Steps

### Task 1.1: Viết test golden (chưa có golden — test phải fail)

Lưu ý: `proxy/testdata/` **chưa tồn tại** trong repo. Nhánh `-update-responses-golden`
trong test gọi `os.MkdirAll` để tạo — đừng bỏ dòng đó, và đừng tự `mkdir` tay.

- [ ] **Step 1: Tạo `proxy/responses_upstream_test.go`**

Test này chụp body mà `kiroPayloadToCodexResponsesRequest` sinh ra cho một payload cố
định, so với file golden. Chạy trước refactor để sinh golden, chạy lại sau refactor —
khớp nghĩa là hành vi Codex không đổi.

```go
package proxy

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateResponsesGolden rewrites the golden file instead of comparing against it.
// Used once before the Phase 01 refactor, and any time the Codex request shape is
// intentionally changed.
var updateResponsesGolden = flag.Bool("update-responses-golden", false,
	"rewrite proxy/testdata/codex_responses_body.golden.json")

// TestCodexResponsesBodyGolden pins the exact request body the Codex dialect
// produces for a payload covering every branch: system priming pair, history with
// tool use, tool result, image, assistant text, and inference config.
//
// The body is produced by the shared builder through the Codex wrapper, so this
// test fails if moving the builder to responses_upstream.go changes a single byte
// of Codex behaviour.
func TestCodexResponsesBodyGolden(t *testing.T) {
	payload := goldenResponsesPayload()
	body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
	if err != nil {
		t.Fatalf("build codex responses body: %v", err)
	}
	got, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "codex_responses_body.golden.json")
	if *updateResponsesGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-responses-golden first): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("codex responses body changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
```

- [ ] **Step 2: Viết `goldenResponsesPayload()`**

Mở `proxy/external_codex_test.go`, đọc `:230-270`, `:300-330` và `:420-445` để lấy cách
dựng payload đúng cho hàm này (tên struct, cách set `ToolSpecification`, `ToolUses`,
`ToolResults`). Dùng lại đúng cách đó, nhưng mở rộng để phủ hết các nhánh:

- `History` có priming pair: `[0]` user chứa system prompt, `[1]` assistant có nội dung
  chứa chuỗi `"i will follow"` (điều kiện ở `external_codex.go:528`) → phải tách thành
  `instructions`.
- `History` có một assistant message kèm `ToolUses` → sinh `function_call` item.
- `History` có một user message kèm `UserInputMessageContext.ToolResults` → sinh
  `function_call_output` item.
- `CurrentMessage.UserInputMessage` có `Content` + `Images` → sinh message item với
  content dạng mảng.
- `CurrentMessage.UserInputMessage.UserInputMessageContext.Tools` có ít nhất 1 tool.
- `InferenceConfig` có `Temperature`, `TopP`, `ReasoningEffort`.
- `OriginalModel` đặt `"gpt-5.6-sol"`.

Đặt hàm này trong `responses_upstream_test.go`, không đặt trong file test khác — golden
phải tự chứa, không phụ thuộc helper của test file khác (helper đó đổi thì golden đổi
theo mà không ai ngờ).

- [ ] **Step 3: Chạy test để xác nhận nó fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCodexResponsesBodyGolden -v
```

Kỳ vọng: FAIL với `read golden (run with -update-responses-golden first)`.

- [ ] **Step 4: Sinh golden từ code HIỆN TẠI (trước refactor)**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCodexResponsesBodyGolden -v -update-responses-golden
```

Kỳ vọng: PASS, log `wrote testdata/codex_responses_body.golden.json`.

- [ ] **Step 5: Đọc golden để xác nhận nó phủ đúng các nhánh**

```bash
cd /Users/van/Tools/OmniProxy && head -60 proxy/testdata/codex_responses_body.golden.json
```

Kỳ vọng thấy đủ: `"instructions"`, `"function_call"`, `"function_call_output"`,
`"reasoning"`, `"tools"`, `"store": false`, `"stream": true`. Nếu thiếu nhánh nào thì
sửa `goldenResponsesPayload()`, chạy lại Step 4.

- [ ] **Step 6: Commit golden TRƯỚC khi refactor**

```bash
cd /Users/van/Tools/OmniProxy && git add proxy/responses_upstream_test.go proxy/testdata/codex_responses_body.golden.json && git commit -m "test(codex): pin responses request body as golden before dialect extraction"
```

Commit riêng bước này là có chủ đích: golden sinh từ code cũ phải nằm trong lịch sử
trước khi code đổi, để diff của bước sau chỉ chứa thay đổi thật.

### Task 1.2: Tạo `proxy/responses_upstream.go`

- [ ] **Step 1: Tạo file với options struct**

```go
// Package proxy — shared translation between the KiroPayload intermediate
// representation and the OpenAI Responses API, in both directions.
//
// The Responses wire shape is the same whether the upstream is OpenAI's own
// /v1/responses, a ChatGPT Codex subscription backend, or a resale gateway that
// speaks Responses. What differs is a small set of behaviours, carried here as
// responsesDialectOptions so callers share one implementation instead of
// drifting copies.
package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"omniproxy/config"
	"strings"
)

// responsesDialectOptions carries the differences between upstreams that speak
// the Responses API. Everything else about building a request and parsing a
// response is shared.
type responsesDialectOptions struct {
	// DefaultModel is used when the payload carries no model ID. Empty means a
	// payload without a model is an error rather than a silent substitution —
	// a generic gateway has no sensible default to guess.
	DefaultModel string

	// ForwardSamplingParams sends temperature and top_p. The ChatGPT Codex
	// backend rejects both with HTTP 400 ("Unsupported parameter: temperature")
	// on GPT-5.x reasoning models, so Codex leaves this false. Generic
	// OpenAI-compatible gateways normally accept them.
	ForwardSamplingParams bool

	// ToolDescription rewrites a tool description before it is sent. Codex uses
	// it to inject CLI lifecycle guidance; nil passes the description through.
	ToolDescription func(name, description string) string
}

// toolDescription applies the dialect's description rewriting, if any.
func (o responsesDialectOptions) toolDescription(name, description string) string {
	if o.ToolDescription == nil {
		return description
	}
	return o.ToolDescription(name, description)
}
```

- [ ] **Step 2: Chuyển hàm dựng body**

Cắt toàn bộ `kiroPayloadToCodexResponsesRequest` (`external_codex.go:496-654`, gồm cả
comment) sang file mới. Đổi tên thành:

```go
func kiroPayloadToResponsesRequest(payload *KiroPayload, account *config.Account, opts responsesDialectOptions) (map[string]interface{}, error) {
```

Sửa đúng 3 chỗ bên trong:

**(a)** Thay khối model mặc định (`:509-511`):

```go
	if modelID == "" {
		modelID = opts.DefaultModel
	}
	if modelID == "" {
		return nil, fmt.Errorf("responses request: payload carries no model id")
	}
```

**(b)** Thay lời gọi tool description (`:633`):

```go
				"description": opts.toolDescription(name, tw.ToolSpecification.Description),
```

**(c)** Thay khối `InferenceConfig` (`:643-651`):

```go
	if payload.InferenceConfig != nil {
		if payload.InferenceConfig.ReasoningEffort != "" {
			body["reasoning"] = map[string]string{"effort": payload.InferenceConfig.ReasoningEffort}
		}
		// Sampling parameters are opt-in per dialect: the ChatGPT Codex backend
		// rejects temperature/top_p with HTTP 400 for GPT-5.x reasoning models,
		// while a generic OpenAI-compatible gateway accepts them.
		if opts.ForwardSamplingParams {
			if payload.InferenceConfig.Temperature > 0 {
				body["temperature"] = payload.InferenceConfig.Temperature
			}
			if payload.InferenceConfig.TopP > 0 {
				body["top_p"] = payload.InferenceConfig.TopP
			}
		}
	}
```

Giữ nguyên comment gốc ở đầu hàm (mô tả shape `input` items), cập nhật tên hàm trong
comment nếu có nhắc.

- [ ] **Step 3: Chuyển `codexToolChoice` và `codexMessageContent`**

Cắt `codexToolChoice` (`:656-696`) và `codexMessageContent` (`:710-748`) sang file mới,
đổi tên thành `responsesToolChoice` và `responsesMessageContent`. Cập nhật các lời gọi
bên trong hàm dựng body. Không sửa logic.

- [ ] **Step 4: Chuyển phần parse SSE**

Cắt `parseCodexResponsesSSE` (`:741-748` comment + `:749-837`), `codexToolAccum`
(`:838-842`), `codexSSELineResult` (`:844-847`), `processCodexSSELine` (`:849-1002`) sang
file mới, đổi tên:

| Tên cũ | Tên mới |
|---|---|
| `parseCodexResponsesSSE` | `parseResponsesSSE` |
| `processCodexSSELine` | `processResponsesSSELine` |
| `codexToolAccum` | `responsesToolAccum` |
| `codexSSELineResult` | `responsesSSELineResult` |

Cập nhật mọi tham chiếu chéo bên trong nhóm này. Không sửa logic.

**Ngoại lệ duy nhất được phép đổi chữ, không đổi luồng:** cuối hàm parse SSE có

```go
		return fmt.Errorf("codex SSE stream ended before response.completed")
```

(`external_codex.go:823`). Sau khi hàm thành dùng chung, chữ `codex` ở đây sai ngữ cảnh —
lỗi này hiện ra trong log khi một gateway **không phải Codex** trả stream cụt. Đổi thành:

```go
		return fmt.Errorf("SSE stream ended before response.completed")
```

Đây là thay đổi văn bản lỗi duy nhất được phép trong phase này. Không có control flow nào
đổi, và golden test không phủ chuỗi lỗi. Nếu `rg -n 'codex SSE stream ended' proxy/` còn
kết quả sau bước này thì chưa đổi.

- [ ] **Step 5: Chuyển `parseCodexResponsesJSON` và `codexBufferedReadCloser`**

Cắt `parseCodexResponsesJSON` (`:1001-1004` comment + hàm) → `parseResponsesJSON`.
Cắt `codexBufferedReadCloser` (`:734-740`) → `bufferedReadCloser`.

- [ ] **Step 6: Build để lộ hết tham chiếu chưa cập nhật**

```bash
cd /Users/van/Tools/OmniProxy && go build ./... 2>&1 | head -40
```

Kỳ vọng: lỗi `undefined: parseCodexResponsesSSE` v.v. ở `external_codex.go` và
`codex_cache_test.go`. Đó là danh sách việc cần sửa ở task sau.

### Task 1.3: Nối lại `external_codex.go`

- [ ] **Step 1: Thêm options Codex**

Thêm vào `external_codex.go`, gần chỗ hàm dựng body cũ:

```go
// defaultCodexResponsesModel is the model substituted when a Codex-bound payload
// carries no model id.
const defaultCodexResponsesModel = "gpt-5.6-sol"

// codexResponsesOptions returns the dialect options for the ChatGPT Codex
// subscription backend: a hard model default, no sampling parameters (the
// backend rejects them for GPT-5.x reasoning models), and the CLI lifecycle
// guidance injected into every tool description.
func codexResponsesOptions() responsesDialectOptions {
	return responsesDialectOptions{
		DefaultModel:          defaultCodexResponsesModel,
		ForwardSamplingParams: false,
		ToolDescription:       codexToolDescription,
	}
}

// kiroPayloadToCodexResponsesRequest is the Codex-flavoured entry point. It is
// kept as a named wrapper because callers and tests address the Codex dialect
// directly, while the translation itself lives in responses_upstream.go.
func kiroPayloadToCodexResponsesRequest(payload *KiroPayload, account *config.Account) (map[string]interface{}, error) {
	return kiroPayloadToResponsesRequest(payload, account, codexResponsesOptions())
}
```

- [ ] **Step 2: Cập nhật lời gọi trong `CallExternalCodex`**

`external_codex.go:400` và `:416`:

```go
		return parseResponsesSSE(resp.Body, callback)
```

```go
		return parseResponsesSSE(&bufferedReadCloser{Reader: br, Closer: resp.Body}, callback)
```

`external_codex.go:419`:

```go
	return parseResponsesJSON(br, callback)
```

- [ ] **Step 3: Cập nhật `codex_cache_test.go`**

Thay ở `:8` (comment), `:58`, `:83`:

```go
			toolAccums := map[string]*responsesToolAccum{}
			processResponsesSSELine("data: "+tc.data, cb, toolAccums, &inTok, &outTok)
```

```go
	if err := parseResponsesJSON(body, cb); err != nil {
		t.Fatalf("parseResponsesJSON: %v", err)
	}
```

- [ ] **Step 4: Build sạch**

```bash
cd /Users/van/Tools/OmniProxy && go build ./... && go vet ./proxy/ 2>&1 | head -20
```

Kỳ vọng: không output.

### Task 1.4: Xác nhận hành vi Codex không đổi

- [ ] **Step 1: Chạy golden — phải khớp y nguyên**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCodexResponsesBodyGolden -v
```

Kỳ vọng: **PASS**, không đọc lại golden. Nếu FAIL, diff chính là hành vi Codex bị đổi —
sửa code cho khớp, **tuyệt đối không** chạy `-update-responses-golden` để cho qua.

- [ ] **Step 2: Chạy toàn bộ test Codex**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'Codex|Responses|codex' -v 2>&1 | tail -30
```

Kỳ vọng: PASS toàn bộ. Nếu `codex_cache_test.go` / `external_codex_test.go` có assert trên
chuỗi `"codex SSE stream ended"` thì cập nhật theo chuỗi mới — đó là hệ quả của thay đổi
ở Task 1.2 Step 4, không phải regression.

- [ ] **Step 3: Chạy toàn bộ package proxy**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ 2>&1 | tail -20
```

Kỳ vọng: PASS. Nếu panic vì bind port, chạy với sandbox cho phép local binding.

- [ ] **Step 4: Commit**

```bash
cd /Users/van/Tools/OmniProxy && git add proxy/responses_upstream.go proxy/external_codex.go proxy/codex_cache_test.go && git commit -m "refactor(codex): extract shared Responses API translation into responses_upstream"
```

## Todo List

- [ ] 1.1 Test golden + sinh golden từ code cũ + commit riêng
- [ ] 1.2 Tạo `responses_upstream.go` (options + builder + parser)
- [ ] 1.3 Nối lại `external_codex.go` (options Codex + wrapper + call sites)
- [ ] 1.4 Golden khớp, test Codex xanh, commit

## Success Criteria

- `proxy/testdata/codex_responses_body.golden.json` được commit **trước** commit refactor.
- Golden do code cũ sinh ra khớp y nguyên sau refactor, không regenerate.
- `go build ./...` và `go vet ./proxy/` sạch.
- `go test ./proxy/` xanh, đặc biệt `external_codex_test.go` và `codex_cache_test.go`.
- `rg -n 'func kiroPayloadToCodexResponsesRequest' proxy/` chỉ còn 1 kết quả (wrapper).
- `rg -n 'codex SSE stream ended' proxy/` không kết quả (chuỗi lỗi đã trung tính hoá).

## Risk Assessment

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Refactor đổi body Codex → hỏng account Codex đang chạy | **Cao** | Golden test; cấm regenerate golden ở Task 1.4 Step 1 |
| Bỏ sót một tham chiếu chéo khi đổi tên | Vừa | `go build ./...` làm danh sách việc, không đoán |
| Golden không phủ hết nhánh → test xanh giả | Vừa | Task 1.1 Step 5 kiểm bằng mắt các key bắt buộc phải có |
| Đổi tên làm hỏng test ngoài dự kiến | Thấp | Task 1.2 Step 6 build để lộ toàn bộ; `rg` trước khi đổi |

## Security Considerations

Không có thay đổi về auth, credential, hay bề mặt mạng. Chỉ di chuyển code trong package.
Không log thêm dữ liệu người dùng.

## Next Steps

Phase 02 phụ thuộc phase này chỉ ở chỗ `defaultExternalResponsesPath` sẽ nằm cùng file
với adapter mới — không phụ thuộc code cụ thể nào. Có thể chạy Phase 02 song song sau khi
Task 1.3 xong build sạch.
