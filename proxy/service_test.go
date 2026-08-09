package proxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"omniproxy/auth"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchAdaptersUseNativeContracts(t *testing.T) {
	initConfigForTests(t)

	tests := []struct {
		name     string
		provider string
		request  func(*testing.T, *http.Request)
		response string
		call     func(*http.Request, *config.Account, searchRequest) (normalizedSearchResponse, error)
		input    searchRequest
		want     string
	}{
		{
			name:     "tavily",
			provider: "tavily",
			input:    searchRequest{Query: "golang", MaxResults: 3, IncludeAnswer: true},
			request: func(t *testing.T, r *http.Request) {
				if r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer tavily-key" {
					t.Fatalf("unexpected Tavily request: path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				var body map[string]interface{}
				decodeServiceJSON(t, r, &body)
				if body["query"] != "golang" || body["max_results"] != float64(3) || body["include_answer"] != true {
					t.Fatalf("unexpected Tavily body: %#v", body)
				}
			},
			response: `{"query":"golang","answer":"Go","results":[{"title":"Go","url":"https://go.dev","content":"A language"}]}`,
			call:     callTavily,
			want:     "tavily",
		},
		{
			name:     "exa",
			provider: "exa",
			input:    searchRequest{Query: "golang", MaxResults: 2, IncludeRawContent: true},
			request: func(t *testing.T, r *http.Request) {
				if r.URL.Path != "/search" || r.Header.Get("x-api-key") != "exa-key" || r.Header.Get("Authorization") != "" {
					t.Fatalf("unexpected Exa headers/path: path=%q x-api-key=%q authorization=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
				}
				var body map[string]interface{}
				decodeServiceJSON(t, r, &body)
				contents, ok := body["contents"].(map[string]interface{})
				if body["query"] != "golang" || body["numResults"] != float64(2) || !ok || contents["text"] != true {
					t.Fatalf("unexpected Exa body: %#v", body)
				}
			},
			response: `{"results":[{"title":"Go","url":"https://go.dev","text":"A language","score":0.9,"publishedDate":"2026-01-01"}]}`,
			call:     callExa,
			want:     "exa",
		},
		{
			name:     "firecrawl",
			provider: "firecrawl",
			input:    searchRequest{Query: "golang", Limit: 4},
			request: func(t *testing.T, r *http.Request) {
				if r.URL.Path != "/v1/search" || r.Header.Get("Authorization") != "Bearer firecrawl-key" {
					t.Fatalf("unexpected Firecrawl request: path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				var body map[string]interface{}
				decodeServiceJSON(t, r, &body)
				if body["query"] != "golang" || body["limit"] != float64(4) {
					t.Fatalf("unexpected Firecrawl body: %#v", body)
				}
			},
			response: `{"data":[{"title":"Go","url":"https://go.dev","description":"desc","markdown":"# Go"}]}`,
			call:     callFirecrawl,
			want:     "firecrawl",
		},
		{
			name:     "jina-reader",
			provider: "jina-reader",
			input:    searchRequest{URL: "https://example.com/docs?a=1"},
			request: func(t *testing.T, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/https://example.com/docs" || r.URL.RawQuery != "a=1" {
					t.Fatalf("unexpected Jina request: method=%s path=%q query=%q", r.Method, r.URL.Path, r.URL.RawQuery)
				}
				if r.Header.Get("Authorization") != "Bearer jina-reader-key" {
					t.Fatalf("Jina authorization = %q", r.Header.Get("Authorization"))
				}
			},
			response: "# Example\n\nReader output",
			call:     callJinaReader,
			want:     "jina-reader",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.request(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()

			baseURL := server.URL
			if tc.provider == "firecrawl" {
				baseURL += "/v1"
			}
			account := &config.Account{Provider: tc.provider, AccessToken: tc.provider + "-key", BaseURL: baseURL}
			got, err := tc.call(httptest.NewRequest(http.MethodPost, "/v1/search", nil), account, tc.input)
			if err != nil {
				t.Fatalf("adapter returned error: %v", err)
			}
			if got.Provider != tc.want || len(got.Results) != 1 {
				t.Fatalf("normalized response = %#v", got)
			}
			if tc.provider == "jina-reader" && !strings.Contains(got.Results[0].Content, "Reader output") {
				t.Fatalf("Jina content = %q", got.Results[0].Content)
			}
		})
	}
}

func TestFirecrawlBaseURLAcceptsRootAndV1(t *testing.T) {
	initConfigForTests(t)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"title":"Go","url":"https://go.dev"}]}`)
	}))
	defer server.Close()

	for _, baseURL := range []string{server.URL, server.URL + "/v1"} {
		account := &config.Account{Provider: "firecrawl", AccessToken: "firecrawl-key", BaseURL: baseURL}
		if _, err := callFirecrawl(httptest.NewRequest(http.MethodPost, "/v1/search", nil), account, searchRequest{Query: "golang"}); err != nil {
			t.Fatalf("callFirecrawl(%q): %v", baseURL, err)
		}
	}
	if len(paths) != 2 || paths[0] != "/v1/search" || paths[1] != "/v1/search" {
		t.Fatalf("Firecrawl paths = %#v, want [/v1/search /v1/search]", paths)
	}
}

func TestHandleSearchFailsOverBetweenAccounts(t *testing.T) {
	initConfigForTests(t)
	var badCalls, goodCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			badCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"query":"q","results":[{"title":"ok","url":"https://example.com"}]}`)
	}))
	defer server.Close()

	for _, account := range []config.Account{
		{ID: "search-bad", Provider: "tavily", AccessToken: "bad-key", BaseURL: server.URL, Capabilities: []string{"search"}, ProviderKind: "search", Enabled: true},
		{ID: "search-good", Provider: "tavily", AccessToken: "good-key", BaseURL: server.URL, Capabilities: []string{"search"}, ProviderKind: "search", Enabled: true},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account: %v", err)
		}
	}
	p := getServiceTestPool(t)
	h := &Handler{pool: p}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"provider":"tavily","query":"q"}`))
		rec := httptest.NewRecorder()
		h.handleSearch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if badCalls.Load() == 0 || goodCalls.Load() < 2 {
		t.Fatalf("failover calls = bad:%d good:%d", badCalls.Load(), goodCalls.Load())
	}
}

func TestImageGenerationAdapterAndHandler(t *testing.T) {
	initConfigForTests(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer image-token" {
			t.Fatalf("unexpected image request: path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.Header.Get("chatgpt-account-id") != "chatgpt-account" {
			t.Fatalf("chatgpt-account-id = %q", r.Header.Get("chatgpt-account-id"))
		}
		var body map[string]interface{}
		decodeServiceJSON(t, r, &body)
		if body["model"] != defaultCodexImageHostModel || body["stream"] != true || body["store"] != false {
			t.Fatalf("unexpected image body: %#v", body)
		}
		if body["instructions"] != "You must fulfill the request by using the image_generation tool." {
			t.Fatalf("unexpected image instructions: %#v", body["instructions"])
		}
		tools, ok := body["tools"].([]interface{})
		if !ok || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "image_generation" {
			t.Fatalf("unexpected image tools: %#v", body["tools"])
		}
		tool := tools[0].(map[string]interface{})
		if tool["model"] != "gpt-image-test" || tool["size"] != "1024x1024" || tool["quality"] != "medium" {
			t.Fatalf("unexpected image tool configuration: %#v", tool)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZmFrZS1pbWFnZQ==\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\ndata: [DONE]\n")
	}))
	defer server.Close()

	account := config.Account{ID: "codex-image-account", AuthMethod: "codex", AccessToken: "image-token", ChatGPTAccountID: "chatgpt-account", BaseURL: server.URL, CodexImageModel: "gpt-image-test", Enabled: true}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add image account: %v", err)
	}
	p := getServiceTestPool(t)
	h := &Handler{pool: p}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"a tree"}`))
	rec := httptest.NewRecorder()
	h.handleImageGeneration(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("image status = %d: %s", rec.Code, rec.Body.String())
	}
	var got imageGenerationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode image response: %v", err)
	}
	if got.Created == 0 || len(got.Data) != 1 || got.Data[0].B64JSON != "ZmFrZS1pbWFnZQ==" {
		t.Fatalf("image response = %#v", got)
	}
}

func TestNormalizeCodexImageAcceptsDataURLsAndArtifacts(t *testing.T) {
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"data:image/png;base64,ZmFrZQ==\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_result\",\"artifact_url\":\"https://cdn.example.test/image.png\"}}\n\n")

	got, err := normalizeCodexImage(raw, "text/event-stream")
	if err != nil {
		t.Fatalf("normalize Codex image: %v", err)
	}
	if len(got.Data) != 2 || got.Data[0].B64JSON != "ZmFrZQ==" || got.Data[1].URL != "https://cdn.example.test/image.png" {
		t.Fatalf("normalized image data = %#v", got.Data)
	}
}

func TestAdminTestAccountCodexImageUsesNativeImageRoute(t *testing.T) {
	initConfigForTests(t)
	config.SetPassword("admin-test-password")

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected Codex path: %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer image-token" || r.Header.Get("chatgpt-account-id") != "chatgpt-account" {
			t.Fatalf("unexpected Codex auth headers: authorization=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("chatgpt-account-id"))
		}
		var body map[string]interface{}
		decodeServiceJSON(t, r, &body)
		if body["model"] != defaultCodexImageHostModel || body["stream"] != true || body["store"] != false {
			t.Fatalf("unexpected image test body: %#v", body)
		}
		if body["instructions"] != "You must fulfill the request by using the image_generation tool." {
			t.Fatalf("unexpected image test instructions: %#v", body["instructions"])
		}
		tools, ok := body["tools"].([]interface{})
		if !ok || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "image_generation" {
			t.Fatalf("unexpected image test tools: %#v", body["tools"])
		}
		tool := tools[0].(map[string]interface{})
		if tool["model"] != "gpt-image-test" || tool["size"] != "1024x1024" || tool["quality"] != "medium" {
			t.Fatalf("unexpected image test tool configuration: %#v", tool)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZmFrZS1pbWFnZQ==\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\ndata: [DONE]\n")
	}))
	defer server.Close()

	if err := config.AddAccount(config.Account{
		ID: "codex-admin-image", AuthMethod: "codex", AccessToken: "image-token",
		RefreshToken: "refresh-token-that-must-not-be-used", ChatGPTAccountID: "chatgpt-account",
		BaseURL: server.URL, CodexImageModel: "gpt-image-test", Enabled: true,
	}); err != nil {
		t.Fatalf("add Codex account: %v", err)
	}
	h := &Handler{pool: getServiceTestPool(t)}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/codex-admin-image/test", strings.NewReader(`{"capability":"image","prompt":"a tree"}`))
	req.Header.Set("X-Admin-Password", "admin-test-password")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin image test status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode admin image test response: %v", err)
	}
	if got["success"] != true || got["capability"] != "image" || got["model"] != "gpt-image-test" || got["imageCount"] != float64(1) {
		t.Fatalf("unexpected admin image test response: %#v", got)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

type blockingImageReader struct {
	data    []byte
	release <-chan struct{}
	reads   int
}

func (r *blockingImageReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return copy(p, r.data), nil
	}
	<-r.release
	return 0, io.EOF
}

func TestReadCodexImageResponseStopsAtCompletionWithoutWaitingForEOF(t *testing.T) {
	release := make(chan struct{})
	body := &blockingImageReader{
		data: []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"ZmFrZQ==\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"),
		release: release,
	}

	resultCh := make(chan struct {
		result imageGenerationResponse
		err    error
	}, 1)
	go func() {
		result, err := readCodexImageResponse(body, "")
		resultCh <- struct {
			result imageGenerationResponse
			err    error
		}{result: result, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil || len(result.result.Data) != 1 || result.result.Data[0].B64JSON != "ZmFrZQ==" {
			t.Fatalf("image result = %#v, err = %v", result.result, result.err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("image parser waited for EOF after response.completed")
	}
	close(release)
}

func TestAdminRestoreCodexRefreshTokenPersistsOnlyValidatedAccount(t *testing.T) {
	tests := []struct {
		name          string
		upstreamCode  int
		upstreamID    string
		wantStatus    int
		wantPersisted bool
	}{
		{
			name:          "success",
			upstreamCode:  http.StatusOK,
			upstreamID:    "acct-expected",
			wantStatus:    http.StatusOK,
			wantPersisted: true,
		},
		{
			name:          "different chatgpt account",
			upstreamCode:  http.StatusOK,
			upstreamID:    "acct-other",
			wantStatus:    http.StatusConflict,
			wantPersisted: false,
		},
		{
			name:          "upstream rejects backup token",
			upstreamCode:  http.StatusBadRequest,
			wantStatus:    http.StatusBadGateway,
			wantPersisted: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initConfigForTests(t)
			config.SetPassword("admin-test-password")

			backupToken := "backup-token-" + strings.ReplaceAll(tc.name, " ", "-")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					t.Fatalf("unexpected OAuth path %q", r.URL.Path)
				}
				if err := r.ParseForm(); err != nil {
					t.Fatalf("parse OAuth form: %v", err)
				}
				if got := r.Form.Get("grant_type"); got != "refresh_token" {
					t.Fatalf("grant_type = %q, want refresh_token", got)
				}
				if got := r.Form.Get("refresh_token"); got != backupToken {
					t.Fatalf("refresh_token did not use supplied backup token")
				}
				if tc.upstreamCode != http.StatusOK {
					w.WriteHeader(tc.upstreamCode)
					_, _ = io.WriteString(w, `{"error":"invalid refresh token"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"`+serviceTestCodexJWT(tc.upstreamID)+`","refresh_token":"rotated-refresh","expires_in":3600}`)
			}))
			defer server.Close()

			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("parse test server URL: %v", err)
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme = target.Scheme
				clone.URL.Host = target.Host
				return transport.RoundTrip(clone)
			})})
			t.Cleanup(func() {
				auth.SetGlobalAuthClientForTest(previousClient)
				transport.CloseIdleConnections()
			})

			const accountID = "codex-restore-account"
			if err := config.AddAccount(config.Account{
				ID:               accountID,
				Email:            "restore@example.com",
				AuthMethod:       codexAuthMethod,
				AccessToken:      "old-access",
				RefreshToken:     "old-refresh",
				ChatGPTAccountID: "acct-expected",
				Enabled:          true,
			}); err != nil {
				t.Fatalf("add Codex account: %v", err)
			}

			pool := getServiceTestPool(t)
			h := &Handler{pool: pool}
			req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+accountID+"/restore-refresh-token", strings.NewReader(`{"refreshToken":"`+backupToken+`"}`))
			req.Header.Set("X-Admin-Password", "admin-test-password")
			rec := httptest.NewRecorder()
			h.handleAdminAPI(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), backupToken) || strings.Contains(rec.Body.String(), "rotated-refresh") {
				t.Fatalf("response must not expose refresh tokens: %s", rec.Body.String())
			}

			persisted := config.GetAccounts()[0]
			pooled := pool.GetByID(accountID)
			if tc.wantPersisted {
				if persisted.AccessToken == "old-access" || persisted.RefreshToken != "rotated-refresh" {
					t.Fatalf("persisted account was not rotated: %#v", persisted)
				}
				if pooled == nil || pooled.RefreshToken != "rotated-refresh" {
					t.Fatalf("pool did not receive recovered token: %#v", pooled)
				}
			} else {
				if persisted.AccessToken != "old-access" || persisted.RefreshToken != "old-refresh" {
					t.Fatalf("failed recovery changed persisted account: %#v", persisted)
				}
				if pooled == nil || pooled.AccessToken != "old-access" || pooled.RefreshToken != "old-refresh" {
					t.Fatalf("failed recovery changed pool account: %#v", pooled)
				}
			}
		})
	}
}

func serviceTestCodexJWT(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]interface{}{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID},
	})
	if err != nil {
		panic(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}

func TestAdminTestAccountExternalImageUsesOpenAIImagesRoute(t *testing.T) {
	initConfigForTests(t)
	config.SetPassword("admin-test-password")

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("unexpected external image path: %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer external-image-key" {
			t.Fatalf("unexpected external image authorization: %q", r.Header.Get("Authorization"))
		}
		var body map[string]interface{}
		decodeServiceJSON(t, r, &body)
		if body["model"] != "provider-image-model" || body["prompt"] != "a mountain" || body["n"] != float64(1) {
			t.Fatalf("unexpected external image body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"url":"https://example.com/image.png"},{"b64_json":"ZmFrZQ=="}]}`)
	}))
	defer server.Close()

	if err := config.AddAccount(config.Account{
		ID: "external-admin-image", AuthMethod: "external_openai", AccessToken: "external-image-key",
		BaseURL: server.URL, ImageModel: "provider-image-model", Enabled: true,
	}); err != nil {
		t.Fatalf("add external image account: %v", err)
	}
	h := &Handler{pool: getServiceTestPool(t)}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/external-admin-image/test", strings.NewReader(`{"capability":"image","prompt":"a mountain"}`))
	req.Header.Set("X-Admin-Password", "admin-test-password")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("external admin image status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode external admin image response: %v", err)
	}
	if got["success"] != true || got["capability"] != "image" || got["model"] != "provider-image-model" || got["imageCount"] != float64(2) {
		t.Fatalf("unexpected external admin image response: %#v", got)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("external image upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestAdminTestAccountKiroImageReportsUnsupportedWithoutChat(t *testing.T) {
	initConfigForTests(t)
	config.SetPassword("admin-test-password")

	if err := config.AddAccount(config.Account{
		ID: "kiro-admin-image", AuthMethod: "social", AccessToken: "kiro-token", Enabled: true,
	}); err != nil {
		t.Fatalf("add Kiro account: %v", err)
	}
	h := &Handler{pool: getServiceTestPool(t)}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-admin-image/test", strings.NewReader(`{"capability":"image","prompt":"a mountain"}`))
	req.Header.Set("X-Admin-Password", "admin-test-password")
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Kiro image status = %d, want %d: %s", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode Kiro image response: %v", err)
	}
	if got["success"] != false || got["capability"] != "image" || got["unsupported"] != true {
		t.Fatalf("unexpected Kiro image response: %#v", got)
	}
}

func TestSearchServiceStatsCaptureUpstreamHeaders(t *testing.T) {
	initConfigForTests(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Limit", "100")
			w.Header().Set("X-RateLimit-Remaining", "99")
			w.Header().Set("X-RateLimit-Reset", "1700000100")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"query":"q","results":[]}`)
			return
		}
		w.Header().Set("X-Rate-Limit-Remaining", "0")
		w.Header().Set("X-Rate-Limit-Reset", "1700000200")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"quota"}`)
	}))
	defer server.Close()

	account := config.Account{ID: "search-stats", Provider: "tavily", AccessToken: "search-token", BaseURL: server.URL, Enabled: true, ProviderKind: "search", Capabilities: []string{"search"}}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add search account: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
	if _, err := callTavily(request, &account, searchRequest{Query: "q"}); err != nil {
		t.Fatalf("successful search: %v", err)
	}
	if _, err := callTavily(request, &account, searchRequest{Query: "q"}); err == nil {
		t.Fatal("expected quota error")
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	got := accounts[0]
	if got.ServiceRequestCount != 2 || got.ServiceErrorCount != 1 || got.ServiceQuotaErrorCount != 1 || got.ServiceLastStatus != http.StatusTooManyRequests {
		t.Fatalf("service counters = %+v", got)
	}
	if got.ServiceRateLimit != "100" || got.ServiceRateLimitRemaining != "0" || got.ServiceRateLimitReset != "1700000200" || got.ServiceRetryAfter != "60" {
		t.Fatalf("service headers = %+v", got)
	}
}

func TestNormalizeOpenRouterImageSupportsBase64Content(t *testing.T) {
	got, err := normalizeOpenRouterImage([]byte(`{"choices":[{"message":{"content":[{"type":"image","b64_json":"ZmFrZQ=="}]}}]}`))
	if err != nil {
		t.Fatalf("normalize image: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].B64JSON != "ZmFrZQ==" {
		t.Fatalf("normalized image = %#v", got)
	}
}

func TestCapabilityRoutingSeparatesServiceAccountsFromChat(t *testing.T) {
	initConfigForTests(t)
	accounts := []config.Account{
		{ID: "chat-only", Enabled: true},
		{ID: "search-only", Provider: "tavily", ProviderKind: "search", Capabilities: []string{"search"}, Enabled: true},
		{ID: "image-only", Provider: "openrouter", ProviderKind: "image", Capabilities: []string{"image"}, Enabled: true},
		{ID: "chat-image", Provider: "openrouter", ProviderKind: "image", Capabilities: []string{"chat", "image"}, Enabled: true},
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %q: %v", account.ID, err)
		}
	}

	p := getServiceTestPool(t)
	if got := p.GetNextForCapability("search", "tavily", nil); got == nil || got.ID != "search-only" {
		t.Fatalf("search account = %#v, want search-only", got)
	}
	if got := p.GetNextForCapability("image", "openrouter", nil); got == nil {
		t.Fatal("expected an image account")
	}
	if got := p.GetNext(); got == nil || (got.ID != "chat-only" && got.ID != "chat-image") {
		t.Fatalf("chat account = %#v, want chat-only or chat-image", got)
	}
}

func decodeServiceJSON(t *testing.T, r *http.Request, target interface{}) {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode request JSON: %v; body=%s", err, data)
	}
}

func getServiceTestPool(t *testing.T) *accountpool.AccountPool {
	t.Helper()
	p := accountpool.GetPool()
	p.Reload()
	return p
}
