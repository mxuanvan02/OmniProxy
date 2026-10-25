package proxy

import (
	"bytes"
	"encoding/json"
	"omniproxy/logger"
	"strings"
)

// extractServedModel reads the model a provider claims in its own response.
// A resale relay that silently substitutes a cheaper model is invisible in
// every log today: the combo asked for A, the provider answered with B, and
// the billing record only carries the requested name. The first chunk of an
// OpenAI chat-completions stream and the top level of both JSON shapes carry
// the served model, so the buffered response the combo layer already holds is
// enough to observe it. Empty means "no claim observed", which is distinct
// from a claim and is never reported as a mismatch.
func extractServedModel(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			if m := modelFieldFromJSON(payload); m != "" {
				return m
			}
		}
		return ""
	}
	return modelFieldFromJSON(trimmed)
}

func modelFieldFromJSON(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// normaliseModelIdentity removes the parts of a model string that legitimately
// vary without changing which model answered: letter case and bracketed
// deployment tags such as claude-opus-5[1m]. Thinking suffixes are resolved by
// the router before a combo attempt starts, so they do not reach this point.
func normaliseModelIdentity(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.Index(m, "["); idx >= 0 {
		m = m[:idx]
	}
	return strings.TrimSpace(m)
}

// servedModelMismatch reports whether the provider's claimed model disagrees
// with the model the request asked for, after variant normalisation. An empty
// served value means the provider made no claim; that is a gap in the
// evidence, not evidence of substitution, so it is not a mismatch.
func servedModelMismatch(requested, served string) bool {
	if strings.TrimSpace(served) == "" {
		return false
	}
	return normaliseModelIdentity(requested) != normaliseModelIdentity(served)
}

// warnServedModelMismatch logs an observable warning when a combo attempt's
// response names a different model than the one requested. It is log-only by
// design: a wrong warning must not break a working answer, and enforcement
// (blocking, routing away) is a later decision that needs a live evidence
// base first. Fail-open: any error here must never affect the response.
func warnServedModelMismatch(comboName, requested string, body []byte) {
	served := extractServedModel(body)
	if served == "" || !servedModelMismatch(requested, served) {
		return
	}
	logger.Warnf("[COMBO] %s model=%s SERVED-MODEL MISMATCH: provider answered with %q — possible relay substitution",
		comboName, requested, served)
}
