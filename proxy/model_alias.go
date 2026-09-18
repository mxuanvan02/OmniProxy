package proxy

import (
	"omniproxy/config"
	"omniproxy/logger"
)

// resolveModelAlias returns the model to route when the exact requested model
// has no healthy account, or model unchanged when it does. It rescues in two
// steps, each cheaper and narrower than the next:
//
//  1. Deployment variants — locale/snapshot spellings of the SAME model
//     (qwen3.8-max vs qwen3.8-max-cn). Behaviour variants (-agent,
//     -thinking-agent) are never substituted; the client must ask for those.
//  2. Cross-family fallbacks — the operator-configured Config.ModelFallbacks
//     list, for a request the pool never served at all (a Claude name
//     advertised via ExtraModels on a Qwen-only pool). Only runs when the list
//     is non-empty, so the default install keeps its historical 503.
//
// The probe only runs once exact routing has already failed, so the happy path
// pays nothing.
func (h *Handler) resolveModelAlias(model string) string {
	if model == "" || h.pool == nil {
		return model
	}
	if h.pool.HasAvailableAccountForModel(model) {
		return model
	}
	if alt := h.pool.FindAvailableAliasModel(model); alt != "" {
		logger.Infof("[ModelAlias] %s -> %s (exact model unavailable)", model, alt)
		return alt
	}
	if alt := h.pool.FindAvailableFallbackModel(config.GetModelFallbacks()); alt != "" {
		logger.Infof("[ModelFallback] %s -> %s (no account serves it; cross-family fallback)", model, alt)
		return alt
	}
	return model
}
