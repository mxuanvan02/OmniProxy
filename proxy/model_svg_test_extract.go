package proxy

import "strings"

// extractSVG finds the first <svg ...>...</svg> block in s, stripping any
// surrounding markdown fences the model may have added despite the prompt.
// It returns "" when there is no complete <svg>…</svg> pair, including when the
// reply was cut off mid-markup — callers use classifySVGReply to tell those
// cases apart rather than reporting a bare "no SVG".
func extractSVG(s string) string {
	// Strip common markdown fence wrappers.
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"```svg", "```xml", "```html", "```"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			s = strings.TrimSpace(s)
			break
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	lower := strings.ToLower(s)
	start := strings.Index(lower, "<svg")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(lower, "</svg>")
	if end < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(s[start : end+len("</svg>")])
}

// classifySVGReply pairs extractSVG with a human-readable reason for failure.
// An animated SVG that hit the output-token ceiling starts with <svg but never
// closes, which looks identical to "the model refused" if we only test for an
// empty string. Distinguishing them keeps the comparison honest: a truncated
// reply means "raise the budget or simplify the prompt", not "this provider
// can't draw".
func classifySVGReply(content string) (svg string, reason string) {
	if svg = extractSVG(content); svg != "" {
		return svg, ""
	}
	lower := strings.ToLower(content)
	switch {
	case strings.TrimSpace(content) == "":
		return "", "empty reply from model"
	case strings.Contains(lower, "<svg"):
		return "", "SVG was cut off before </svg> — output likely hit the token cap"
	default:
		return "", "reply contained no SVG markup"
	}
}
