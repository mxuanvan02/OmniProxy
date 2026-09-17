package proxy

import (
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"testing"
)

// Pure tests for the model-level retry decision. The httptest-backed walk they
// enable lives in capability_probe_model_retry_test.go.

func TestModelUnreachableForProbe(t *testing.T) {
	cases := []struct {
		name   string
		status int
		detail string
		want   bool
	}{
		{
			// The exact shape VIBE10 returned: a per-model grant missing on a
			// credential that otherwise works.
			name:   "no grant for this model",
			status: 403,
			detail: `{"message":"This token has no access to model qwen3.8-max-thinking-agent","type":"new_api_error"}`,
			want:   true,
		},
		{
			// The original 503: the distributor has no upstream for this model.
			name:   "distributor has no channel",
			status: 503,
			detail: `{"message":"No available channel for model deepseek-v4-flash-vision-exp under group VIBE8 (distributor)","code":"model_not_found"}`,
			want:   true,
		},
		{
			name:   "quota exhausted",
			status: 429,
			detail: `{"code":"request_quota_exceeded","message":"Request allowance exhausted."}`,
			want:   true,
		},
		{
			// A 400 is the upstream rejecting our body. That is evidence about
			// the capability, and retrying another model would erase it.
			name:   "bad request is a real verdict",
			status: 400,
			detail: `{"error":{"message":"images are not supported by this model"}}`,
			want:   false,
		},
		{
			name:   "endpoint absent is a real verdict",
			status: 404,
			detail: `{"detail":"Not Found"}`,
			want:   false,
		},
		{
			// A bare 403 with no model wording is a credential problem: no other
			// model is going to answer on a dead key.
			name:   "credential forbidden",
			status: 403,
			detail: `{"message":"invalid api key"}`,
			want:   false,
		},
		{
			name:   "empty body carries no model signal",
			status: 502,
			detail: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelUnreachableForProbe(tc.status, tc.detail); got != tc.want {
				t.Errorf("modelUnreachableForProbe(%d, %q) = %v, want %v", tc.status, tc.detail, got, tc.want)
			}
		})
	}
}

// Vision is a property of a chat model, not a family of its own, so the picker
// must offer chat models and nothing else — and must offer the plain model
// before its variants, because the pool caches each catalog as a set and map
// order would otherwise make the probe report differently on every run.
func TestNewProbeModelPickerFiltersToFamilyInOrder(t *testing.T) {
	config.Init("")
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{ID: "acc-picker-filter", Enabled: true}
	// Qwen3-Embedding-8B is the only non-chat model real catalogs list; a bare
	// "flux-1" image model stands in for the same case. The suffixed variant is
	// listed first on purpose: rank, not catalog order, decides who is tried.
	h.pool.SetModelList(account.ID, []string{
		"qwen3.8-max-thinking-agent", "Qwen3-Embedding-8B", "qwen3.8-max", "flux-1",
	})

	picker := h.newProbeModelPicker(account, capabilityVision)
	if picker.exhausted != "" {
		t.Fatalf("picker exhausted with chat models present: %s", picker.exhausted)
	}
	var got []string
	for {
		model, more := picker.next()
		if !more {
			break
		}
		got = append(got, model)
	}
	want := []string{"qwen3.8-max", "qwen3.8-max-thinking-agent"}
	if len(got) != len(want) {
		t.Fatalf("picker offered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picker offered %v, want %v (the plain model must come before its variants)", got, want)
		}
	}
}

// A catalog with no model of the family is a definitive answer, so the picker
// reports it instead of leaving the caller with an empty result and no reason.
func TestNewProbeModelPickerReportsEmptyFamily(t *testing.T) {
	config.Init("")
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{ID: "acc-picker-empty", Enabled: true}
	h.pool.SetModelList(account.ID, []string{"Qwen3-Embedding-8B"})

	picker := h.newProbeModelPicker(account, capabilityVision)
	if picker.exhausted == "" {
		t.Fatal("picker with no chat model reports no reason, so the probe cannot say why it skipped")
	}
	if _, more := picker.next(); more {
		t.Error("picker returned a model although it is exhausted")
	}
}

// The budget is what stops one diagnostic from becoming a long run of billed
// requests against a gateway that denies everything it is asked for.
func TestProbeModelRetriesAreBounded(t *testing.T) {
	if maxProbeModelRetries < 2 {
		t.Errorf("maxProbeModelRetries = %d, want at least 2: one retry cannot recover from a denied first variant", maxProbeModelRetries)
	}
	if maxProbeModelRetries > 5 {
		t.Errorf("maxProbeModelRetries = %d, want at most 5: a probe is not worth many paid calls", maxProbeModelRetries)
	}
}
