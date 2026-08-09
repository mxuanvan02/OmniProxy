package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	flowInputTokens  = 950
	flowOutputTokens = 7
	flowCacheRead    = 800
	flowCacheCreate  = 50
)

// TestCacheUsageFlowsThroughPublicChatEndpoints exercises the production
// request path: client handler -> account pool -> CallKiroAPI -> AWS event
// stream parser -> callback -> client response and UsageTracker. It also
// asserts exactly one upstream request so a cache warm-up cannot silently be
// reintroduced alongside the real request.
func TestCacheUsageFlowsThroughPublicChatEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name             string
		path             string
		body             string
		invoke           func(*Handler, http.ResponseWriter, *http.Request)
		assertFn         func(*testing.T, []byte)
		endpoint         string
		openAIStyleCache bool
	}{
		{
			name: "claude messages",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			invoke: func(h *Handler, w http.ResponseWriter, r *http.Request) {
				h.handleClaudeMessages(w, r)
			},
			assertFn: func(t *testing.T, body []byte) {
				t.Helper()
				var response ClaudeResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode Claude response: %v; body=%s", err, body)
				}
				if response.Usage.InputTokens != flowInputTokens-flowCacheRead-flowCacheCreate ||
					response.Usage.OutputTokens != flowOutputTokens ||
					response.Usage.CacheReadInputTokens != flowCacheRead ||
					response.Usage.CacheCreationInputTokens != flowCacheCreate {
					t.Fatalf("unexpected Claude usage: %+v", response.Usage)
				}
			},
			endpoint: endpointClaude,
		},
		{
			name: "openai chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}]}`,
			invoke: func(h *Handler, w http.ResponseWriter, r *http.Request) {
				h.handleOpenAIChat(w, r)
			},
			assertFn: func(t *testing.T, body []byte) {
				t.Helper()
				var response struct {
					Usage struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
						TotalTokens      int `json:"total_tokens"`
						PromptDetails    struct {
							CachedTokens int `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode OpenAI response: %v; body=%s", err, body)
				}
				if response.Usage.PromptTokens != flowInputTokens ||
					response.Usage.CompletionTokens != flowOutputTokens ||
					response.Usage.TotalTokens != flowInputTokens+flowOutputTokens ||
					response.Usage.PromptDetails.CachedTokens != flowCacheRead {
					t.Fatalf("unexpected OpenAI usage: %+v", response.Usage)
				}
			},
			endpoint:         endpointOpenAI,
			openAIStyleCache: true,
		},
		{
			name: "openai responses",
			path: "/v1/responses",
			body: `{"model":"claude-sonnet-4.5","input":"hello","store":false}`,
			invoke: func(h *Handler, w http.ResponseWriter, r *http.Request) {
				h.handleOpenAIResponses(w, r)
			},
			assertFn: func(t *testing.T, body []byte) {
				t.Helper()
				var response ResponsesObject
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode Responses response: %v; body=%s", err, body)
				}
				if response.Usage.InputTokens != flowInputTokens ||
					response.Usage.OutputTokens != flowOutputTokens ||
					response.Usage.TotalTokens != flowInputTokens+flowOutputTokens ||
					response.Usage.InputTokensDetails == nil ||
					response.Usage.InputTokensDetails.CachedTokens != flowCacheRead {
					t.Fatalf("unexpected Responses usage: %+v", response.Usage)
				}
			},
			endpoint:         endpointOpenAIResponses,
			openAIStyleCache: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamRequests int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := atomic.AddInt32(&upstreamRequests, 1); got > 1 {
					t.Errorf("unexpected extra upstream request %d: cache warm-up must not run", got)
				}
				if r.Method != http.MethodPost {
					t.Errorf("upstream method = %s, want POST", r.Method)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
					"content": "cached upstream reply",
					"usage": map[string]interface{}{
						"uncachedInputTokens":   float64(100),
						"cacheReadInputTokens":  float64(flowCacheRead),
						"cacheWriteInputTokens": float64(flowCacheCreate),
						"outputTokens":          float64(flowOutputTokens),
					},
				}))
			}))
			defer upstream.Close()

			h, tracker := newCacheUsageFlowHandler(t, tc.name)
			defer swapKiroEndpointsForTest(t, upstream)()

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			recorder := httptest.NewRecorder()
			tc.invoke(h, recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := atomic.LoadInt32(&upstreamRequests); got != 1 {
				t.Fatalf("upstream requests = %d, want exactly 1", got)
			}
			tc.assertFn(t, recorder.Body.Bytes())
			assertTrackedUpstreamCacheUsage(t, tracker, tc.endpoint, tc.openAIStyleCache)
		})
	}
}

func newCacheUsageFlowHandler(t *testing.T, accountID string) (*Handler, *UsageTracker) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "cache-flow-" + strings.ReplaceAll(accountID, " ", "-"),
		Email:       "cache-flow@example.test",
		Enabled:     true,
		AccessToken: "token-test",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/cache-flow",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	return &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: tracker,
	}, tracker
}

func assertTrackedUpstreamCacheUsage(t *testing.T, tracker *UsageTracker, endpoint string, openAIStyleCache bool) {
	t.Helper()
	stats := tracker.GetStats("all")
	if len(stats.RecentRequests) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(stats.RecentRequests))
	}
	record := stats.RecentRequests[0]
	if record.Endpoint != endpoint || record.InputTokens != flowInputTokens || record.OutputTokens != flowOutputTokens ||
		record.CacheCreateTokens != flowCacheCreate || record.CacheSource != "upstream" {
		t.Fatalf("unexpected usage record: %+v", record)
	}
	if openAIStyleCache {
		if record.CachedTokens != flowCacheRead || record.CacheReadTokens != 0 ||
			stats.TotalCachedTokens != flowCacheRead || stats.TotalCacheReadTokens != 0 ||
			stats.TotalUpstreamCachedTokens != flowCacheRead || stats.TotalUpstreamCacheReadTokens != 0 {
			t.Fatalf("unexpected OpenAI-style cache usage: record=%+v stats=%+v", record, stats)
		}
	} else if record.CacheReadTokens != flowCacheRead || record.CachedTokens != 0 ||
		stats.TotalCacheReadTokens != flowCacheRead || stats.TotalCachedTokens != 0 ||
		stats.TotalUpstreamCacheReadTokens != flowCacheRead || stats.TotalUpstreamCachedTokens != 0 {
		t.Fatalf("unexpected Claude-style cache usage: record=%+v stats=%+v", record, stats)
	}
	if stats.TotalCacheCreateTokens != flowCacheCreate ||
		stats.TotalUpstreamCacheCreateTokens != flowCacheCreate {
		t.Fatalf("unexpected aggregated cache usage: %+v", stats)
	}
	summary := stats.ByEndpoint[endpoint]
	if summary == nil || summary.CacheCreateTokens != flowCacheCreate ||
		summary.UpstreamCacheCreateTokens != flowCacheCreate {
		t.Fatalf("unexpected endpoint cache summary: %+v", summary)
	}
	if openAIStyleCache {
		if summary.CachedTokens != flowCacheRead || summary.UpstreamCachedTokens != flowCacheRead ||
			summary.CacheReadTokens != 0 || summary.UpstreamCacheReadTokens != 0 {
			t.Fatalf("unexpected OpenAI-style endpoint cache summary: %+v", summary)
		}
	} else if summary.CacheReadTokens != flowCacheRead || summary.UpstreamCacheReadTokens != flowCacheRead ||
		summary.CachedTokens != 0 || summary.UpstreamCachedTokens != 0 {
		t.Fatalf("unexpected Claude-style endpoint cache summary: %+v", summary)
	}
}
