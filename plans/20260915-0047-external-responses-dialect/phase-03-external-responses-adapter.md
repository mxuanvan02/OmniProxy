# Phase 03 — Adapter Responses cho external provider

**Priority:** P0
**Trạng thái:** Chưa làm
**Ước lượng:** ~4 giờ
**Phụ thuộc:** Phase 01 (helper dùng chung), Phase 02 (field + nhánh rẽ)

## Context Links

- Spec: [`design.md`](design.md) §5.3, §5.4, §7
- Adapter chat để đối chiếu: `proxy/external_openai.go:264-324` (`CallExternalOpenAI`)
- Transport tái dùng: `setExternalOpenAIHeaders` (`:69`), `doExternalOpenAIRequest` (`:182`), `openAICompatibleEndpoint` (`:1648`)
- Sniff SSE/JSON để đối chiếu: `proxy/external_codex.go:395-419`
- Gate chống stream rỗng: `proxy/external_openai.go:1060-1154`

## Overview

Thay stub ở Phase 02 bằng adapter thật: dựng body Responses, POST tới
`{BaseURL}{ResponsesPath}`, parse SSE hoặc JSON fallback, đẩy qua `KiroStreamCallback`.

## Key Insights

- **Transport dùng lại 100% từ đường chat.** Cùng `setExternalOpenAIHeaders` (kể cả
  profile `curl` cho gateway chặn fingerprint SDK), cùng `doExternalOpenAIRequest` (đã
  có WAF detect + tự thử lại bằng curl identity), cùng `GetClientForProxy` +
  `ResolveAccountProxyURL`. Không viết transport mới.
- **Sniff SSE phải peek, không chỉ tin `Content-Type`.** `external_codex.go:395-419` đã
  giải quyết đúng: nhiều upstream bỏ `Content-Type` hoặc trả type chung chung. Peek 1
  byte; `{` → JSON, còn lại → SSE. Quan trọng: **phải truyền `bufio.Reader` đã peek cho
  parser**, đọc thẳng `resp.Body` sau `Peek` sẽ mất bytes đang nằm trong buffer và biến
  một stream hợp lệ thành stream rỗng.
- **`blankOutputGate` là wrapper tự chứa.** `newBlankOutputGate(callback).callback()`
  trả về một `*KiroStreamCallback` đã gate; `flush()` được gọi từ bên trong `onText` và
  nhánh tool, không cần gọi kết thúc thủ công. Nghĩa là adapter wrap callback **trước
  khi** đưa vào parser dùng chung — không phải sửa parser, nên hành vi Codex không đổi.
- **Codex không có gate này.** Đó là lý do gate nằm ở adapter chứ không nằm trong
  `parseResponsesSSE`.
- **Không tự rơi về chat khi lỗi.** Spec §7 chốt vậy. Một gateway trả 404 cho
  `/v1/responses` nghĩa là dialect bị cấu hình sai — im lặng gọi lại bằng chat sẽ che mất
  lỗi cấu hình và làm số liệu token vô nghĩa, đúng bài toán tính năng này sinh ra để giải.

## Requirements

**Functional**
- `CallExternalOpenAIResponses` dựng body bằng `kiroPayloadToResponsesRequest` với
  `externalResponsesOptions()`.
- `stream: true` luôn được set; Responses không có `stream_options` tương đương chat —
  usage về trên `response.completed`.
- Endpoint: `openAICompatibleEndpoint(baseURL, externalResponsesPath(account))`.
- `Content-Type` chứa `text/event-stream` → `parseResponsesSSE`.
- Ngược lại: peek; `{` → `parseResponsesJSON`, còn lại → `parseResponsesSSE` qua
  `bufferedReadCloser`.
- Callback được bọc `blankOutputGate` trước khi vào parser.
- Status != 200 → lỗi kèm body đã truncate; 401/403/402 giữ nguyên để tầng pool xử lý
  auth-failure như đường chat.

**Non-functional**
- Không log `AccessToken`.
- Lỗi nêu rõ dialect để debug: thông báo phải phân biệt được "sai dialect" với "gateway hỏng".

## Architecture

```
dispatchChat
  └── externalAPIDialect == "responses"
        └── CallExternalOpenAIResponses
              ├── kiroPayloadToResponsesRequest(payload, account, externalResponsesOptions())
              ├── openAICompatibleEndpoint(baseURL, externalResponsesPath(account))
              ├── setExternalOpenAIHeaders + doExternalOpenAIRequest   (tái dùng)
              ├── blankOutputGate(callback)
              └── parseResponsesSSE | parseResponsesJSON              (dùng chung)
```

## Related Code Files

**Sửa**
- `proxy/external_openai_responses.go` — thay stub, thêm `externalResponsesOptions()`

**Sửa test**
- `proxy/external_openai_responses_test.go` — thêm test integration

**Không đụng**
- `proxy/external_codex.go`, `proxy/responses_upstream.go` — phase này không sửa

## Implementation Steps

### Task 3.1: Options cho external + adapter thật

- [ ] **Step 1: Viết test fail**

Thêm vào `proxy/external_openai_responses_test.go`. Test dùng lại `goldenResponsesPayload()`
từ Phase 01 — cùng package, payload đã phủ đủ nhánh.

```go
func TestCallExternalOpenAIResponsesSendsResponsesShape(t *testing.T) {
	var gotPath, gotAuth, gotUA string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "ext-responses",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}

	var text strings.Builder
	callback := &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if !isThinking {
				text.WriteString(s)
			}
		},
	}

	if err := CallExternalOpenAIResponses(context.Background(), account, goldenResponsesPayload(), callback); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}

	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "OpenAI/Python") {
		t.Fatalf("User-Agent = %q, want the OpenAI SDK identity", gotUA)
	}
	// The Responses shape carries "input" items, never chat "messages".
	if _, ok := gotBody["messages"]; ok {
		t.Fatal("body carried chat-completions messages; the Responses shape was not used")
	}
	if _, ok := gotBody["input"]; !ok {
		t.Fatal("body has no input items")
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Fatal("stream_options has no meaning in the Responses API and must not be sent")
	}
	if text.String() != "pong" {
		t.Fatalf("callback text = %q, want %q", text.String(), "pong")
	}
}
```

- [ ] **Step 2: Chạy test để xác nhận fail**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCallExternalOpenAIResponsesSendsResponsesShape -v
```

Kỳ vọng: FAIL — `responses dialect not implemented yet` (stub Phase 02).

- [ ] **Step 3: Thêm options external**

Thêm vào `proxy/external_openai_responses.go`:

```go
// externalResponsesOptions returns the dialect options for a generic
// OpenAI-compatible gateway that speaks the Responses API.
//
// Unlike the ChatGPT Codex backend, such a gateway normally accepts temperature
// and top_p, so they are forwarded. No model default is set: a gateway has no
// sensible model to guess, and sending a wrong one is worse than a clear error.
// No tool-description rewriting either — the Codex lifecycle guidance describes
// Codex CLI tooling and would be misleading to another client.
func externalResponsesOptions() responsesDialectOptions {
	return responsesDialectOptions{
		ForwardSamplingParams: true,
	}
}
```

- [ ] **Step 4: Thay stub bằng implementation**

Xóa hàm `CallExternalOpenAIResponses` stub và thay bằng:

```go
// CallExternalOpenAIResponses forwards a KiroPayload to an external
// OpenAI-compatible provider using the Responses API dialect.
//
// A gateway that only speaks Responses has to translate chat completions into
// Responses internally, and that translation loses the reasoning items the model
// would otherwise carry between turns. Sending the Responses shape directly
// avoids the lossy hop.
func CallExternalOpenAIResponses(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("external responses call: account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("external account %s has no baseUrl", account.Email)
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return fmt.Errorf("external account %s has no apiKey", account.Email)
	}

	body, err := kiroPayloadToResponsesRequest(payload, account, externalResponsesOptions())
	if err != nil {
		return fmt.Errorf("external responses call build request: %w", err)
	}
	// Always stream: the handler's non-stream path buffers through the callback.
	// Responses has no stream_options equivalent — usage arrives on the
	// response.completed event.
	body["stream"] = true

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("external responses call marshal: %w", err)
	}

	endpoint := openAICompatibleEndpoint(baseURL, externalResponsesPath(account))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("external responses call new request: %w", err)
	}
	setExternalOpenAIHeaders(req, account, apiKey, "text/event-stream")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := doExternalOpenAIRequest(client, req, account)
	if err != nil {
		return fmt.Errorf("external responses call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		// 401/403/402 deliberately keep their status in the message so the pool's
		// auth-failure handling can disable the account, exactly as on the chat
		// dialect. No fallback to chat: a gateway answering 404 here means the
		// dialect is misconfigured, and silently retrying as chat would hide that
		// while corrupting the token accounting this dialect exists to improve.
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
	}

	// Withhold whitespace-only text so a turn that never produces real content
	// stays retryable instead of reaching the client as a finished, empty answer.
	// Applied here rather than inside the shared parser because the Codex dialect
	// does not use the gate.
	gate := newBlankOutputGate(callback)
	gated := gate.callback()

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseResponsesSSE(resp.Body, gated)
	}
	// Some upstreams omit Content-Type on an SSE body. Peek to tell a JSON
	// fallback from a stream, and hand the buffered reader to whichever parser is
	// chosen — reading resp.Body after Peek would drop the buffered bytes.
	br := bufio.NewReader(resp.Body)
	first, err := br.Peek(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("external responses peek: %w", err)
	}
	if len(first) == 0 || first[0] != '{' {
		return parseResponsesSSE(&bufferedReadCloser{Reader: br, Closer: resp.Body}, gated)
	}
	return parseResponsesJSON(br, gated)
}
```

Cập nhật import block của file:

```go
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"strings"
)
```

- [ ] **Step 5: Chạy test để xác nhận pass**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCallExternalOpenAIResponsesSendsResponsesShape -v
```

Kỳ vọng: PASS.

### Task 3.2: Path override

- [ ] **Step 1: Viết test fail**

```go
// A Codex-style reseller commonly serves Responses behind a prefix rather than at
// /v1/responses, so the account override is the difference between the dialect
// working and returning 404.
func TestCallExternalOpenAIResponsesHonoursPathOverride(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n")
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "ext-responses-path",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
		ResponsesPath:      "/codex/responses",
	}

	if err := CallExternalOpenAIResponses(context.Background(), account, goldenResponsesPayload(), &KiroStreamCallback{}); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}
	if gotPath != "/codex/responses" {
		t.Fatalf("upstream path = %q, want /codex/responses", gotPath)
	}
}
```

- [ ] **Step 2: Chạy test**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCallExternalOpenAIResponsesHonoursPathOverride -v
```

Kỳ vọng: PASS ngay — `externalResponsesPath` đã làm ở Phase 02. Nếu FAIL thì lời gọi
trong adapter chưa dùng `externalResponsesPath`, sửa lại.

### Task 3.3: JSON fallback và lỗi không tự rơi về chat

- [ ] **Step 1: Viết test JSON fallback**

```go
// Gateway ignored stream=true and answered a single JSON Responses object. The
// adapter must still deliver the text rather than erroring on the missing SSE.
func TestCallExternalOpenAIResponsesJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Type on purpose: exercises the peek path.
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello json"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "ext-responses-json",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}

	var text strings.Builder
	callback := &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if !isThinking {
				text.WriteString(s)
			}
		},
	}
	if err := CallExternalOpenAIResponses(context.Background(), account, goldenResponsesPayload(), callback); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}
	if !strings.Contains(text.String(), "hello json") {
		t.Fatalf("callback text = %q, want it to contain %q", text.String(), "hello json")
	}
}
```

Nếu `parseResponsesJSON` mong đợi shape khác (đọc `parseCodexResponsesJSON` ở
`external_codex.go:1004` để xác nhận field nó đọc), chỉnh JSON trong test cho khớp shape
thật. Điều đang kiểm là **adapter chọn đúng parser**, không phải parser hoạt động ra sao —
parser đã có test riêng.

- [ ] **Step 2: Viết test không fallback**

```go
// A 404 on the Responses path means the dialect is misconfigured. The adapter must
// surface it instead of quietly retrying as chat: a silent fallback would hide the
// misconfiguration and make the token accounting this dialect exists for
// meaningless.
func TestCallExternalOpenAIResponsesDoesNotFallBackToChat(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		http.Error(w, "<html>404</html>", http.StatusNotFound)
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "ext-responses-404",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}

	err := CallExternalOpenAIResponses(context.Background(), account, goldenResponsesPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should name the status: %v", err)
	}
	for _, p := range hits {
		if p == "/v1/chat/completions" {
			t.Fatal("adapter fell back to the chat path; a misconfigured dialect must surface")
		}
	}
}
```

- [ ] **Step 4: Chạy cả hai test**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run 'TestCallExternalOpenAIResponses' -v 2>&1 | tail -20
```

Kỳ vọng: PASS toàn bộ.

### Task 3.4: Stream cụt (thiếu `response.completed`)

Spec §9 yêu cầu có test cho ca này: gateway trả `200 OK` rồi đóng stream trước
`response.completed`. Parser dùng chung (`external_codex.go:822-824`, sau Phase 01 chuyển
sang `responses_upstream.go`) **đã** trả lỗi cho ca này — task này chỉ khoá hành vi đó lại
ở tầng adapter, để một lần đổi parser sau này không âm thầm biến stream cụt thành câu trả
lời rỗng.

- [ ] **Step 1: Viết test**

```go
// A gateway that answers 200 and then closes the stream before
// response.completed has produced a truncated turn, not an empty answer. The
// adapter must surface that so the caller can retry or fail over, instead of
// handing the client a blank assistant message that looks finished.
func TestCallExternalOpenAIResponsesRejectsTruncatedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		// No response.completed: the stream just ends here.
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "ext-responses-truncated",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}

	var text strings.Builder
	callback := &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if !isThinking {
				text.WriteString(s)
			}
		},
	}

	err := CallExternalOpenAIResponses(context.Background(), account, goldenResponsesPayload(), callback)
	if err == nil {
		t.Fatal("expected an error for a stream that ends before response.completed")
	}
	if !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("error should name the missing event: %v", err)
	}
	// Text already delivered stays delivered: the gate does not retract content
	// the client has seen, it only withholds an all-whitespace turn.
	if !strings.Contains(text.String(), "partial") {
		t.Fatalf("callback text = %q, want it to contain the delivered delta", text.String())
	}
}
```

- [ ] **Step 2: Chạy test**

```bash
cd /Users/van/Tools/OmniProxy && go test ./proxy/ -run TestCallExternalOpenAIResponsesRejectsTruncatedStream -v
```

Kỳ vọng: PASS. Nếu FAIL vì `err.Error()` không chứa `response.completed`, đọc lại hàm
parse SSE dùng chung — hành vi chặn stream cụt phải nằm ở đó (Phase 01 đã chuyển nó).

- [ ] **Step 3: Chạy toàn package**

```bash
cd /Users/van/Tools/OmniProxy && go build ./... && go vet ./proxy/ && go test ./proxy/ 2>&1 | tail -20
```

Kỳ vọng: build/vet sạch, test PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/van/Tools/OmniProxy && git add proxy/external_openai_responses.go proxy/external_openai_responses_test.go && git commit -m "feat(external): implement Responses API outbound adapter"
```

## Todo List

- [ ] 3.1 `externalResponsesOptions()` + adapter thật (thay stub) + test shape
- [ ] 3.2 Test path override
- [ ] 3.3 Test JSON fallback + test không fallback về chat
- [ ] 3.4 Test stream cụt (thiếu `response.completed`) + commit

## Success Criteria

- Path mặc định `/v1/responses`; override hoạt động.
- Body là shape Responses (`input`), không có `messages`, không có `stream_options`.
- Header identity giống đường chat (SDK fingerprint, hoặc curl khi account set profile).
- SSE và JSON fallback đều tới được callback.
- 404 để lộ lỗi, không gọi `/v1/chat/completions`.
- Stream cụt (200 rồi EOF trước `response.completed`) trả lỗi, không thành câu trả lời rỗng.
- `sseIdleWatchdog` vẫn hoạt động: nó nằm trong parser dùng chung (`external_codex.go:760`
  trước Phase 01), adapter không được bỏ qua nó. Kiểm bằng
  `rg -n 'sseIdleWatchdog' proxy/responses_upstream.go` — phải có kết quả.
- Stub Phase 02 không còn tồn tại: `rg -n 'not implemented yet' proxy/` không kết quả.

## Risk Assessment

| Rủi ro | Mức | Giảm thiểu |
|---|---|---|
| Gateway trả SSE mà không có `response.completed` → stream treo | Vừa | `sseIdleWatchdog` trong parser dùng chung bắt ca treo; ca stream cụt có test riêng ở Task 3.4 |
| Peek sai làm mất bytes đầu → stream rỗng | Vừa | Dùng đúng pattern `external_codex.go:406-416`, có test JSON fallback không set Content-Type |
| `blankOutputGate` chặn nhầm output thật | Thấp | Gate chỉ giữ lại text thuần whitespace, đã chạy ở đường chat; test shape kiểm text tới được callback |
| Gateway từ chối `temperature` dù đã bật forward | Thấp | Lỗi 400 trả về rõ ràng, không im lặng; xử lý ở Phase 05 nếu gặp thật |

## Security Considerations

- Không log `apiKey`; lỗi chỉ nêu email account và status.
- Body lỗi từ upstream đi qua `truncateErrBody` (giới hạn 400 ký tự) — giữ nguyên, không
  đổi để tránh rò rỉ nội dung dài vào log.
- Không mở endpoint nhận request mới; chỉ thay đổi hướng outbound.
- `doExternalOpenAIRequest` giữ nguyên URL guard hiện có (`proxy/urlguard.go`).

## Next Steps

Phase 04 mở field cho người dùng chọn khi add key. Phase 05 đo thật trên gateway.
