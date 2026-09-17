package proxy

import (
	"fmt"
	"net/http"
	"omniproxy/config"
	"omniproxy/pool"
	"sort"
	"strings"
)

// maxProbeModelRetries bounds how many catalog models one probe will try before
// reporting failure. The bound exists so a gateway that denies every model
// cannot turn one diagnostic into a long run of billed requests.
const maxProbeModelRetries = 3

// probeModelPicker walks an account's catalog and hands back the next model of a
// capability's family worth probing.
//
// It exists because a probe that tries exactly one model cannot distinguish "this
// account has no vision" from "this account's credential cannot reach that one
// model". Reseller catalogs are full of variants the key was never granted —
// qwen3.8-max-thinking-agent, qwen3.8-max-fast-agent — and picking the first
// match reported a 403 "no access to model" as a missing capability on an account
// that serves qwen3.8-max perfectly well.
type probeModelPicker struct {
	models []string
	pos    int
	// exhausted carries the reason no model could be offered at all, so the
	// caller can report it instead of an empty result.
	exhausted string
}

// newProbeModelPicker builds a picker for one capability family. Vision maps to
// the chat family: it is a property of a chat model, not a model family of its
// own, and no catalog in this pool carries a token that means "accepts images".
func (h *Handler) newProbeModelPicker(account *config.Account, capability string) *probeModelPicker {
	family := capability
	if capability == capabilityVision {
		family = capabilityChat
	}
	models, reason := probeCandidateModels(h, account, family)
	return &probeModelPicker{models: models, exhausted: reason}
}

// next returns the next candidate model, or false when the list is used up.
func (p *probeModelPicker) next() (string, bool) {
	if p.pos >= len(p.models) {
		return "", false
	}
	model := p.models[p.pos]
	p.pos++
	return model, true
}

// probeCandidateModels lists the catalog models belonging to a capability family.
//
// The pool cache is consulted first, but it is only populated for accounts that
// are enabled: a disabled account has an empty cached catalog even though its
// provider still lists models. Relying on the cache alone therefore produced a
// false negative ("no model in cached catalog") that looked identical to a real
// failure. When the cache misses, fetch the provider catalog directly so a probe
// reports evidence about the endpoint rather than about pool bookkeeping.
func probeCandidateModels(h *Handler, account *config.Account, family string) ([]string, string) {
	if account == nil {
		return nil, "account not found"
	}
	if models := h.pool.GetModelList(account.ID); len(models) > 0 {
		out := probeModelsForFamily(models, family)
		if len(out) == 0 {
			// Cache is populated and holds nothing for this capability: that is a
			// definitive answer, no live fetch needed.
			return nil, fmt.Sprintf("provider catalog lists no %s model", family)
		}
		return out, ""
	}

	discovered, err := fetchExternalProviderModels(account)
	if err != nil {
		return nil, fmt.Sprintf("could not fetch provider catalog: %v", err)
	}
	if len(discovered) == 0 {
		return nil, "provider catalog is empty"
	}
	ids := make([]string, 0, len(discovered))
	for _, m := range discovered {
		ids = append(ids, m.ModelId)
	}
	out := probeModelsForFamily(ids, family)
	if len(out) == 0 {
		return nil, fmt.Sprintf("provider catalog lists no %s model", family)
	}
	return out, ""
}

// probeModelsForFamily returns the catalog models belonging to a capability
// family, in the order the walk should try them.
//
// Sorting is not cosmetic. The pool caches each catalog as a set, so
// GetModelList returns map order: without sorting, two runs against the same
// account would probe different models and disagree about the capability.
//
// Base models come before suffixed variants. Reseller catalogs list
// qwen3.8-max-thinking-agent next to qwen3.8-max, a credential is far likelier
// to be granted the plain model, and the plain model is the one production
// traffic actually gets routed to — so it is the one worth reporting on.
func probeModelsForFamily(models []string, family string) []string {
	out := make([]string, 0, len(models))
	for _, id := range models {
		if containsFold(classifyModelCapabilities(id), family) {
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := probeModelRank(out[i]), probeModelRank(out[j]); a != b {
			return a < b
		}
		return out[i] < out[j]
	})
	return out
}

// probeModelRank counts the dash-separated tokens of a normalized model ID, so
// a variant sorts after the model it is derived from.
func probeModelRank(model string) int {
	return strings.Count(normalizeModelIDForClassification(model), "-") + 1
}

// modelUnreachableForProbe reports whether an upstream reply is a verdict about
// the model rather than about the capability, meaning the probe should move on to
// the next model instead of recording a failure.
//
// Three shapes qualify, and each says nothing about whether the endpoint works:
//   - the credential has no grant for this model (403 / model_not_found)
//   - the catalog lists the model but no channel backs it (503 "no available
//     channel") — the same permanent condition the cooldown classifier catches
//   - the account is out of quota right now (429), so every model would fail and
//     retrying only burns requests
//
// A 400 is deliberately absent: that is the upstream rejecting the request body,
// which is real evidence about the capability and must not be retried away.
func modelUnreachableForProbe(status int, detail string) bool {
	lower := strings.ToLower(detail)
	switch {
	case status == http.StatusTooManyRequests:
		return true
	case pool.IsRateLimitError(fmt.Errorf("%s", detail)):
		return true
	case pool.IsProviderModelUnavailableError(fmt.Errorf("%s", detail)):
		return true
	case status == http.StatusForbidden && strings.Contains(lower, "no access to model"):
		return true
	default:
		return false
	}
}
