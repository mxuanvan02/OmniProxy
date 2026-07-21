package proxy

import (
	"encoding/json"
	"net/http"
	"omniproxy/config"
)

// apiGetPoolStrategy GET /admin/api/pool/strategy
// Returns the currently configured pool routing strategy and the minimum
// pool size at which it activates.
func (h *Handler) apiGetPoolStrategy(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"strategy":          config.GetStringSetting("poolRoutingStrategy", "round-robin"),
		"minPoolSize":       20,
		"availableOptions":  []string{"round-robin", "cost-optimized", "reset-aware"},
		"resetAwareLeadMin": 30,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiUpdatePoolStrategy PATCH /admin/api/pool/strategy
// Sets the pool routing strategy. Accepts "round-robin", "cost-optimized",
// or "reset-aware". Invalid values are rejected with 400.
func (h *Handler) apiUpdatePoolStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	switch req.Strategy {
	case "round-robin", "cost-optimized", "reset-aware":
		// valid
	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Invalid strategy. Must be one of: round-robin, cost-optimized, reset-aware",
		})
		return
	}
	config.SetStringSetting("poolRoutingStrategy", req.Strategy)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"strategy":    req.Strategy,
		"minPoolSize": 20,
	})
}
