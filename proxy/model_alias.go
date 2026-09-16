package proxy

import "omniproxy/logger"

// resolveModelAlias returns the deployment variant to route when the exact
// requested model has no healthy account, or model unchanged when it does.
// Deployment variants are locale/snapshot spellings of one model (qwen3.8-max
// vs qwen3.8-max-cn); behaviour variants (-agent, -thinking-agent) are never
// substituted. The probe only runs once exact routing has already failed, so
// the happy path pays nothing.
func (h *Handler) resolveModelAlias(model string) string {
	if model == "" || h.pool == nil {
		return model
	}
	if h.pool.HasAvailableAccountForModel(model) {
		return model
	}
	alt := h.pool.FindAvailableAliasModel(model)
	if alt == "" {
		return model
	}
	logger.Infof("[ModelAlias] %s -> %s (exact model unavailable)", model, alt)
	return alt
}
