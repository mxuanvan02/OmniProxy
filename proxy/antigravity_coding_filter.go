// Package proxy: antigravity_coding_filter.go
//
// Scrubs non-Antigravity coding-tool names from the system prompt before an
// Antigravity (Cloud Code Assist) request goes upstream. Adapted from the
// cpa-plugin-antigravity-coding-filter concept, narrowed to one field: the
// system instruction, which is where a coding client announces itself
// ("You are Claude Code...").
//
// Modes (config.Account.AntigravityCodingFilter):
//
//	""|"off"   no-op (default)
//	"rewrite"  replace each matched name with "Antigravity"
//	"block"    return blocked=true when any name is found
//
// This works around Antigravity's client gating; see external_antigravity.go
// for the Terms-of-Service risk note. Off by default and per-account on purpose.
package proxy

import (
	"regexp"
	"sort"
	"strings"
)

const antigravityFilterReplacement = "Antigravity"

// antigravityCodingKeywords are coding-tool names scrubbed from the system
// prompt. Matched case-insensitively on word-ish boundaries.
var antigravityCodingKeywords = []string{
	"Claude Code", "Codex CLI", "OpenAI Codex", "Codex", "OpenCode",
	"GitHub Copilot", "Copilot CLI", "Copilot",
	"Gemini Code Assist", "Gemini CLI",
	"Cursor", "Windsurf", "Codeium", "Cline", "Roo Code", "Kilo Code",
	"Aider", "Continue.dev",
	"Amazon Q Developer", "CodeWhisperer", "JetBrains AI Assistant", "Junie",
	"Kiro", "Qoder CLI", "Qoder", "Qwen Code", "Trae", "Tabnine",
	"Sourcegraph Cody", "Augment Code", "Replit Agent", "Ghostwriter",
	"Devin", "OpenHands", "SWE-agent", "Goose", "Zed AI", "Void Editor",
	"PearAI", "Refact.ai", "Tabby", "GitLab Duo", "Visual Studio IntelliCode",
	"CodeBuddy", "Blackbox AI", "Pieces for Developers", "Qodo", "CodiumAI",
	"Rovo Dev CLI", "Factory Droid", "OpenClaw", "Clawdbot", "Moltbot",
	"Hermes Agent", "WorkBuddy",
}

// antigravityFilterRegexp is built once from the keyword list, longest name
// first so "Codex CLI" wins over "Codex". Word boundaries keep "Kiro" from
// matching inside an unrelated token.
var antigravityFilterRegexp = buildAntigravityFilterRegexp(antigravityCodingKeywords)

func buildAntigravityFilterRegexp(keywords []string) *regexp.Regexp {
	sorted := make([]string, 0, len(keywords))
	for _, k := range keywords {
		if strings.TrimSpace(k) != "" {
			sorted = append(sorted, k)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i]) > len(sorted[j])
	})
	quoted := make([]string, len(sorted))
	for i, k := range sorted {
		quoted[i] = regexp.QuoteMeta(k)
	}
	// (?i) case-insensitive; \b...\b so a name is matched as a whole token.
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

// applyAntigravityCodingFilter returns the (possibly rewritten) system prompt
// and whether the request should be blocked. mode is the account's
// AntigravityCodingFilter value; anything other than "rewrite"/"block" is a
// no-op.
func applyAntigravityCodingFilter(mode, systemInstruction string) (out string, blocked bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "rewrite":
		return antigravityFilterRegexp.ReplaceAllString(systemInstruction, antigravityFilterReplacement), false
	case "block":
		return systemInstruction, antigravityFilterRegexp.MatchString(systemInstruction)
	default:
		return systemInstruction, false
	}
}
