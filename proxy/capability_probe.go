package proxy

import (
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"sort"
	"strings"
)

// This file holds the admin entry point. The probe itself is split by concern:
//
//	capability_probe_wire.go     which body and path a capability needs
//	capability_probe_walk.go     the catalog walk and how an outcome is recorded
//	capability_probe_models.go   which models are worth asking, in which order
//	capability_probe_request.go  the single request that leaves the process

// apiProbeAccountCapabilities exposes probing over the admin API. By default it
// probes only the cheap capabilities; ?includeCostly=true opts into audio/image
// probes that bill real usage.
func (h *Handler) apiProbeAccountCapabilities(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Account not found",
		})
		return
	}

	includeCostly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("includeCostly")), "true")
	results := h.probeAccountCapabilities(account, includeCostly)

	changed := false
	for capability, result := range results {
		if applyProbeResult(account, capability, result) {
			changed = true
		}
	}
	if changed {
		if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
			logger.Infof("[CapabilityProbe] failed to persist results for %s: %v", account.ID, err)
		}
	}

	// Three outcomes, deliberately kept apart. Folding skipped into failed was
	// the original defect: a probe that never left the process says nothing
	// about the endpoint, yet it rendered identically to a 404 from upstream.
	verified, failed, skipped := summarizeProbeResults(results)

	// An empty result set is not a failure: it means the account advertises no
	// capability that this run was willing to probe. Say so explicitly rather
	// than returning a bare {} the caller has to interpret.
	note := ""
	if len(results) == 0 {
		note = "no advertised capability was eligible for probing"
		if !includeCostly {
			note += " (retry with includeCostly=true to include audio and image)"
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"accountId":     account.ID,
		"includeCostly": includeCostly,
		"probes":        results,
		"verified":      verified,
		"failed":        failed,
		"skipped":       skipped,
		"note":          note,
	})
}

// summarizeProbeResults buckets outcomes for the response and sorts each list so
// two runs over the same account produce byte-identical JSON. The response is
// what an operator reads, and a map iteration order that shuffles it looks like
// the probe itself is unstable.
func summarizeProbeResults(results map[string]config.CapabilityProbeResult) (verified, failed, skipped []string) {
	verified = make([]string, 0, len(results))
	failed = make([]string, 0, len(results))
	skipped = make([]string, 0, len(results))
	for capability, result := range results {
		switch {
		case result.OK:
			verified = append(verified, capability)
		case result.Skipped:
			skipped = append(skipped, capability)
		default:
			failed = append(failed, capability)
		}
	}
	sort.Strings(verified)
	sort.Strings(failed)
	sort.Strings(skipped)
	return verified, failed, skipped
}
