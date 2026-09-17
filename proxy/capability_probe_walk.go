package proxy

import (
	"fmt"
	"omniproxy/config"
	"strings"
	"time"
)

// probeAccountCapability probes one capability and returns the recorded result.
// It never mutates the account; the caller decides whether to persist.
//
// Capabilities that need a model (everything but moderation) walk the catalog
// rather than trying a single model. A reseller credential is routinely denied
// some variants it still lists, and a 403 on the first one is not evidence about
// the capability — see probeModelPicker.
func (h *Handler) probeAccountCapability(account *config.Account, capability string) config.CapabilityProbeResult {
	now := time.Now().Unix()
	result := config.CapabilityProbeResult{CheckedAt: now}

	if account == nil {
		result.Skipped = true
		result.SkippedReason = "account not found"
		result.Detail = result.SkippedReason
		return result
	}
	if !isExternalAccount(account) || strings.TrimSpace(account.BaseURL) == "" {
		result.Skipped = true
		result.SkippedReason = "capability probing applies to OpenAI-compatible providers only"
		result.Detail = result.SkippedReason
		return result
	}
	credential := strings.TrimSpace(account.AccessToken)
	if credential == "" {
		result.Skipped = true
		result.SkippedReason = "account has no credential"
		result.Detail = result.SkippedReason
		return result
	}

	path, ok := probeUpstreamPath(capability)
	if !ok {
		result.Skipped = true
		result.SkippedReason = fmt.Sprintf("%s has no probeable JSON endpoint", capability)
		result.Detail = result.SkippedReason
		return result
	}

	// Moderation accepts a bare input; everything else needs a model ID, so it
	// runs once with an empty model and the picker below is skipped.
	if capability == capabilityModeration {
		return h.probeOnce(account, capability, "", path, credential)
	}

	picker := h.newProbeModelPicker(account, capability)
	if picker.exhausted != "" {
		result.Skipped = true
		result.SkippedReason = picker.exhausted
		result.Detail = picker.exhausted
		return result
	}

	// Try each candidate until one answers. A model-level refusal (no grant, no
	// channel, out of quota) is not a capability verdict, so the probe moves on;
	// the last such result is what gets recorded if no model ever answered.
	attempts := 0
	var lastUnreachable config.CapabilityProbeResult
	for {
		model, more := picker.next()
		if !more {
			break
		}
		if attempts >= maxProbeModelRetries {
			break
		}
		attempts++

		result = h.probeOnce(account, capability, model, path, credential)
		if result.OK {
			return result
		}
		// A genuine capability failure (400 on the body, 404 on the endpoint)
		// is real evidence: record it and stop, rather than masking it behind
		// the next model.
		if !modelUnreachableForProbe(result.Status, result.Detail) {
			return result
		}
		lastUnreachable = result
	}

	// Every candidate was refused at the model level. Report the last refusal
	// but mark it skipped rather than failed: the probe never got a clean shot
	// at the capability, so this is not evidence the endpoint is broken.
	if lastUnreachable.CheckedAt != 0 {
		lastUnreachable.Skipped = true
		lastUnreachable.SkippedReason = fmt.Sprintf(
			"no probeable %s model: every candidate was refused at the model level (last: %s)",
			capability, truncateForLog(lastUnreachable.Detail))
		return lastUnreachable
	}
	result.Skipped = true
	result.SkippedReason = fmt.Sprintf("no %s model could be probed", capability)
	result.Detail = result.SkippedReason
	return result
}

// probeAccountCapabilities probes every advertised capability on the account,
// honouring the cheap/expensive split unless includeCostly is set.
func (h *Handler) probeAccountCapabilities(account *config.Account, includeCostly bool) map[string]config.CapabilityProbeResult {
	out := make(map[string]config.CapabilityProbeResult)
	if account == nil {
		return out
	}
	eligible := effectiveAccountCapabilities(account)
	// Vision is a question about a chat model, not a separate model family, and
	// no reseller catalog in this pool publishes the input-type metadata that
	// discovery needs to answer it. So an account advertising chat is exactly an
	// account whose vision support is unknown — probing it is the only way the
	// matrix ever learns the answer instead of silently reporting nothing.
	if containsFold(eligible, capabilityChat) && !containsFold(eligible, capabilityVision) {
		eligible = append(eligible, capabilityVision)
	}
	for _, capability := range eligible {
		if capability == capabilitySearch {
			// Search providers speak bespoke protocols handled by search.go.
			continue
		}
		if !includeCostly && !probeCapabilityIsCheap(capability) {
			continue
		}
		if _, probeable := probeUpstreamPath(capability); !probeable {
			continue
		}
		out[capability] = h.probeAccountCapability(account, capability)
	}
	return out
}

// applyProbeResult stores a probe outcome on the account, returning true when
// the stored map changed. Only a change in OK, status or model counts: those
// are what the matrix displays, and rewriting the same verdict would bump the
// account's updated-at on every probe run.
func applyProbeResult(account *config.Account, capability string, result config.CapabilityProbeResult) bool {
	if account == nil || strings.TrimSpace(capability) == "" {
		return false
	}
	if account.CapabilityProbes == nil {
		account.CapabilityProbes = make(map[string]config.CapabilityProbeResult)
	}
	previous, existed := account.CapabilityProbes[capability]
	account.CapabilityProbes[capability] = result
	if !existed {
		return true
	}
	return previous.OK != result.OK || previous.Status != result.Status || previous.Model != result.Model
}
