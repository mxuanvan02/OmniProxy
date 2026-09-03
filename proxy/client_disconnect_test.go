package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"testing"
)

func TestClientGoneRecognisesCancellation(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if clientGone(live, nil) {
		t.Fatal("live context with no error reported as client gone")
	}
	if clientGone(live, errors.New("upstream 500")) {
		t.Fatal("a real upstream error on a live context reported as client gone")
	}

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	if !clientGone(dead, nil) {
		t.Fatal("cancelled context not recognised")
	}
	// The error shape http.Client produces, on a context we no longer hold.
	wrapped := fmt.Errorf("post %q: %w", "https://upstream.test", context.Canceled)
	if !clientGone(context.Background(), wrapped) {
		t.Fatal("wrapped context.Canceled not recognised")
	}
}

// TestClientDisconnectDoesNotCooldownPool pins the phase-05 regression: once
// every chat path derives from r.Context(), a client hangup surfaces as
// "context canceled" from each account in turn. No error classifier matches it,
// so the failover loop walked the whole pool and recorded a failure against
// every account. RecordError locks a model after 3 consecutive errors, so three
// abandoned streams were enough to 503 the model for every account at once.
func TestClientDisconnectDoesNotCooldownPool(t *testing.T) {
	initConfigForTests(t)

	const model = "gpt-5.6-sol"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	const accounts = 3
	for i := 0; i < accounts; i++ {
		id := fmt.Sprintf("disconnect-%d", i)
		if err := config.AddAccount(config.Account{
			ID: id, Email: id, AuthMethod: externalAuthMethod,
			AccessToken: "test-key", BaseURL: server.URL, Enabled: true,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()

	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), usageTracker: GetUsageTracker()}

	// RecordError needs 3 consecutive errors per account before it locks the
	// model, so one disconnect is not observable — three are, and three
	// abandoned streams is an entirely ordinary thing for a CLI client to do.
	for attempt := 0; attempt < 3; attempt++ {
		payload := &KiroPayload{OriginalModel: model}
		payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
			Content: "abandoned", ModelID: model, Origin: "AI_EDITOR",
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client is already gone when the handler runs
		h.handleClaudeStream(ctx, httptest.NewRecorder(), payload, model, false,
			claudeThinkingResponseOptions{}, 1, nil, "")
	}

	if got := p.GetNextForModel(model); got == nil {
		t.Fatalf("no account left for %q after three client disconnects: the failover loop recorded each cancellation as an account failure and locked the model pool-wide", model)
	}
}
