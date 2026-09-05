package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"omniproxy/config"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─── Compression settings ───────────────────────────────────────────────────

// CompressionConfig controls tool-output compression, Headroom prompt
// compression, and Caveman terse-output system prompt injection.
type CompressionConfig struct {
	mu                sync.RWMutex
	toolOutputEnabled bool
	headroomEnabled   bool
	headroomURL       string
	cavemanEnabled    bool
	cavemanLevel      string // "off", "light", "full"
	ponytailEnabled   bool
	stats             compressionStats
}

type compressionStats struct {
	mu             sync.Mutex
	toolCompressed int
	toolTokensIn   int
	toolTokensOut  int
	headroomReqs   int
	cavemanReqs    int
}

var globalCompression = &CompressionConfig{
	toolOutputEnabled: false,
	headroomEnabled:   false,
	headroomURL:       "http://localhost:8787",
	cavemanEnabled:    false,
	cavemanLevel:      "full",
	ponytailEnabled:   false,
}

// compressionSnapshot is an immutable copy of the compression settings. Readers
// take one snapshot at request entry and pass it down, so a concurrent
// apiUpdateCompressionConfig cannot tear a string field mid-request or make one
// request observe two different settings generations.
type compressionSnapshot struct {
	toolOutputEnabled bool
	headroomEnabled   bool
	headroomURL       string
	cavemanEnabled    bool
	cavemanLevel      string
	ponytailEnabled   bool
}

// snapshot returns the current settings under the read lock.
func (c *CompressionConfig) snapshot() compressionSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return compressionSnapshot{
		toolOutputEnabled: c.toolOutputEnabled,
		headroomEnabled:   c.headroomEnabled,
		headroomURL:       c.headroomURL,
		cavemanEnabled:    c.cavemanEnabled,
		cavemanLevel:      c.cavemanLevel,
		ponytailEnabled:   c.ponytailEnabled,
	}
}

// InitCompressionConfig loads compression settings from config.
func InitCompressionConfig() {
	globalCompression.mu.Lock()
	defer globalCompression.mu.Unlock()
	// Read from config KVSettings
	globalCompression.toolOutputEnabled = config.GetBoolSetting("compressToolOutput", false)
	globalCompression.headroomEnabled = config.GetBoolSetting("headroomEnabled", false)
	globalCompression.headroomURL = config.GetStringSetting("headroomURL", "http://localhost:8787")
	globalCompression.cavemanEnabled = config.GetBoolSetting("cavemanEnabled", false)
	globalCompression.cavemanLevel = config.GetStringSetting("cavemanLevel", "full")
	globalCompression.ponytailEnabled = config.GetBoolSetting("ponytailEnabled", false)
}

// ─── RTK-style tool output compression ──────────────────────────────────────

// Patterns that identify compressible tool output.
var (
	gitDiffHeaderRe = regexp.MustCompile(`^diff --git a/.*$`)
	gitDiffHunkRe   = regexp.MustCompile(`^@@ -\d+,\d+ \+\d+,\d+ @@.*$`)
	grepFileRe      = regexp.MustCompile(`^(.+):\d+:(.*)$`)
	lsLineRe        = regexp.MustCompile(`^[-dlrwx]{10}.*$`)
)

// compressToolOutput applies RTK-style compression to tool_result content:
//   - git diff: keep headers + hunks, collapse repeated context lines
//   - grep/rg: keep file:line:match format, deduplicate
//   - ls/find: collapse to compact listing
//   - Long repetitive output: collapse runs of identical lines
//
// Returns the compressed content and whether compression was applied.
func compressToolOutput(content string) (string, bool) {
	if !globalCompression.snapshot().toolOutputEnabled {
		return content, false
	}
	if len(content) < 500 {
		// Too small to bother compressing
		return content, false
	}

	original := content
	var result string
	applied := false

	lines := strings.Split(content, "\n")

	// Detect output type and apply appropriate compression
	switch {
	case isGitDiffOutput(lines):
		result, applied = compressGitDiff(lines)
	case isGrepOutput(lines):
		result, applied = compressGrep(lines)
	case isLsOutput(lines):
		result, applied = compressLs(lines)
	default:
		// Generic: collapse runs of 3+ identical lines into [N identical lines]
		result, applied = collapseIdenticalRuns(lines)
	}

	if !applied || len(result) >= len(original) {
		return content, false
	}

	globalCompression.stats.mu.Lock()
	globalCompression.stats.toolCompressed++
	globalCompression.stats.toolTokensIn += estimateTokens(original)
	globalCompression.stats.toolTokensOut += estimateTokens(result)
	globalCompression.stats.mu.Unlock()

	return result, true
}

func isGitDiffOutput(lines []string) bool {
	h := 0
	for i, l := range lines {
		if i > 50 {
			break
		}
		if gitDiffHeaderRe.MatchString(l) || gitDiffHunkRe.MatchString(l) {
			h++
		}
	}
	return h >= 2
}

func isGrepOutput(lines []string) bool {
	m := 0
	for i, l := range lines {
		if i > 50 {
			break
		}
		if grepFileRe.MatchString(l) {
			m++
		}
	}
	return m >= 3
}

func isLsOutput(lines []string) bool {
	m := 0
	for i, l := range lines {
		if i > 30 {
			break
		}
		if lsLineRe.MatchString(l) {
			m++
		}
	}
	return m >= 3
}

// compressGitDiff keeps diff headers and hunk markers, collapses context lines
// that aren't additions/removals. Preserves the semantic content.
func compressGitDiff(lines []string) (string, bool) {
	var out []string
	contextRun := 0
	for _, l := range lines {
		if gitDiffHeaderRe.MatchString(l) || gitDiffHunkRe.MatchString(l) ||
			strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") ||
			strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") ||
			strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "new file") ||
			strings.HasPrefix(l, "deleted file") || strings.HasPrefix(l, "rename ") {
			if contextRun > 3 {
				out = append(out, fmt.Sprintf("  ... [%d context lines collapsed]", contextRun-2))
			}
			contextRun = 0
			out = append(out, l)
		} else {
			// Context line (starts with space or is blank in diff)
			contextRun++
			if contextRun <= 2 {
				out = append(out, l)
			}
		}
	}
	if contextRun > 3 {
		out = append(out, fmt.Sprintf("  ... [%d context lines collapsed]", contextRun-2))
	}
	result := strings.Join(out, "\n")
	return result, len(result) < len(strings.Join(lines, "\n"))
}

// compressGrep keeps file:line:match but deduplicates consecutive matches
// from the same file, showing only unique lines.
func compressGrep(lines []string) (string, bool) {
	var out []string
	seen := make(map[string]bool)
	dupCount := 0
	for _, l := range lines {
		if grepFileRe.MatchString(l) {
			// Extract the match content (after file:line:)
			parts := strings.SplitN(l, ":", 3)
			key := ""
			if len(parts) >= 3 {
				key = parts[2]
			}
			if key != "" && seen[key] {
				dupCount++
				continue
			}
			seen[key] = true
		}
		out = append(out, l)
	}
	if dupCount > 0 {
		out = append(out, fmt.Sprintf("\n[%d duplicate matches collapsed]", dupCount))
	}
	result := strings.Join(out, "\n")
	return result, dupCount > 0
}

// compressLs collapses verbose ls -la output to compact name + size listing.
func compressLs(lines []string) (string, bool) {
	var out []string
	for _, l := range lines {
		if lsLineRe.MatchString(l) {
			// Parse: perms links owner group size date name
			fields := strings.Fields(l)
			if len(fields) >= 9 {
				perms := fields[0]
				size := fields[4]
				name := strings.Join(fields[8:], " ")
				out = append(out, fmt.Sprintf("%s  %8s  %s", perms[:1], size, name))
			} else {
				out = append(out, l)
			}
		} else {
			out = append(out, l)
		}
	}
	result := strings.Join(out, "\n")
	return result, len(result) < len(strings.Join(lines, "\n"))
}

// collapseIdenticalRuns collapses runs of 3+ identical consecutive lines.
func collapseIdenticalRuns(lines []string) (string, bool) {
	var out []string
	i := 0
	collapsed := false
	for i < len(lines) {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		if run >= 3 {
			out = append(out, lines[i])
			out = append(out, fmt.Sprintf("  ... [%d identical lines collapsed]", run-1))
			collapsed = true
		} else {
			for k := i; k < j; k++ {
				out = append(out, lines[k])
			}
		}
		i = j
	}
	result := strings.Join(out, "\n")
	return result, collapsed
}

func estimateTokens(s string) int {
	// Rough: 4 chars per token
	return len(s) / 4
}

// ─── Caveman terse output system prompt ─────────────────────────────────────

// cavemanSystemPromptSuffix is appended to the system prompt when Caveman is
// enabled, biasing the model toward terse output (65-87% fewer output tokens).
var cavemanPromptFull = `
[Output style: ULTRA-TERSE]
- Answer in the fewest words possible. No filler, no preamble, no "Here's...", no restating the question.
- Code: emit only the changed lines with minimal context. No full-file dumps unless explicitly asked.
- Explanations: one sentence max. Use bullet points only if >2 items.
- No markdown headers unless the response is >500 words.
- No code comments unless they encode non-obvious business logic.
- Prefer symbols over words: → not "leads to", = not "equals", × not "times".
- If a list, use inline format: a, b, c — not bulleted.
- Brevity applies to prose only, never to actions. If the task needs a tool, call it in this same turn; a short sentence describing the action is not a substitute for performing it.
`

var cavemanPromptLight = `
[Output style: concise]
- Be concise. No preamble or restating the question.
- Code: show only changed lines unless full context is needed.
- Explanations: 1-2 sentences max.
- Brevity applies to prose only, never to actions. If the task needs a tool, call it in this same turn; a short sentence describing the action is not a substitute for performing it.
`

// cavemanSuffix returns the system prompt suffix for Caveman mode.
func cavemanSuffix(snap compressionSnapshot) string {
	if !snap.cavemanEnabled {
		return ""
	}
	switch snap.cavemanLevel {
	case "light":
		return cavemanPromptLight
	case "full":
		return cavemanPromptFull
	default:
		return cavemanPromptFull
	}
}

// ponytailSuffix biases the model toward minimal code (YAGNI, reuse stdlib).
var ponytailPrompt = `
[Code style: minimal]
- YAGNI: don't add code that isn't needed yet.
- Reuse stdlib and existing utilities before introducing new dependencies.
- Prefer deletion over addition when fixing bugs.
- Smallest correct change. Touch only what needs to change.
`

func ponytailSuffix(snap compressionSnapshot) string {
	if !snap.ponytailEnabled {
		return ""
	}
	return ponytailPrompt
}

// ApplySystemPromptSuffix appends Caveman + Ponytail suffixes to a system prompt.
func ApplySystemPromptSuffix(systemPrompt string) string {
	snap := globalCompression.snapshot()
	suffix := cavemanSuffix(snap) + ponytailSuffix(snap)
	if suffix == "" {
		return systemPrompt
	}
	globalCompression.stats.mu.Lock()
	if snap.cavemanEnabled {
		globalCompression.stats.cavemanReqs++
	}
	globalCompression.stats.mu.Unlock()
	return systemPrompt + suffix
}

// ─── Headroom prompt compression ────────────────────────────────────────────

// compressViaHeadroom sends the request body to Headroom's /v1/compress
// endpoint and returns the compressed body. Returns original on any error.
func compressViaHeadroom(body []byte) ([]byte, bool) {
	snap := globalCompression.snapshot()
	if !snap.headroomEnabled || snap.headroomURL == "" {
		return body, false
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		snap.headroomURL+"/v1/compress",
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return body, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return body, false
	}

	var result struct {
		Messages     []json.RawMessage `json:"messages"`
		TokensBefore int               `json:"tokens_before"`
		TokensAfter  int               `json:"tokens_after"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return body, false
	}

	if len(result.Messages) == 0 {
		return body, false
	}

	globalCompression.stats.mu.Lock()
	globalCompression.stats.headroomReqs++
	globalCompression.stats.mu.Unlock()

	// Re-encode with compressed messages
	var orig map[string]interface{}
	if err := json.Unmarshal(body, &orig); err != nil {
		return body, false
	}
	orig["messages"] = result.Messages
	compressed, err := json.Marshal(orig)
	if err != nil {
		return body, false
	}
	return compressed, true
}

// ─── Compression stats API ──────────────────────────────────────────────────

// apiGetCompressionStats GET /admin/api/compression/stats
func (h *Handler) apiGetCompressionStats(w http.ResponseWriter, r *http.Request) {
	globalCompression.mu.RLock()
	globalCompression.stats.mu.Lock()
	stats := map[string]interface{}{
		"toolOutputEnabled": globalCompression.toolOutputEnabled,
		"headroomEnabled":   globalCompression.headroomEnabled,
		"headroomURL":       globalCompression.headroomURL,
		"cavemanEnabled":    globalCompression.cavemanEnabled,
		"cavemanLevel":      globalCompression.cavemanLevel,
		"ponytailEnabled":   globalCompression.ponytailEnabled,
		"stats": map[string]int{
			"toolCompressed": globalCompression.stats.toolCompressed,
			"toolTokensIn":   globalCompression.stats.toolTokensIn,
			"toolTokensOut":  globalCompression.stats.toolTokensOut,
			"headroomReqs":   globalCompression.stats.headroomReqs,
			"cavemanReqs":    globalCompression.stats.cavemanReqs,
		},
	}
	globalCompression.stats.mu.Unlock()
	globalCompression.mu.RUnlock()
	json.NewEncoder(w).Encode(stats)
}

// apiUpdateCompressionConfig PATCH /admin/api/compression/config
func (h *Handler) apiUpdateCompressionConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToolOutputEnabled *bool   `json:"toolOutputEnabled,omitempty"`
		HeadroomEnabled   *bool   `json:"headroomEnabled,omitempty"`
		HeadroomURL       *string `json:"headroomURL,omitempty"`
		CavemanEnabled    *bool   `json:"cavemanEnabled,omitempty"`
		CavemanLevel      *string `json:"cavemanLevel,omitempty"`
		PonytailEnabled   *bool   `json:"ponytailEnabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Apply in memory first, release the lock, then persist. Each config setter
	// holds cfgLock across a full marshal + fsync of config.json, and readers
	// take mu.RLock via snapshot() — Go's RWMutex blocks new readers once a
	// writer is waiting, so persisting under mu would stall every in-flight
	// request for six fsyncs.
	globalCompression.mu.Lock()
	if req.ToolOutputEnabled != nil {
		globalCompression.toolOutputEnabled = *req.ToolOutputEnabled
	}
	if req.HeadroomEnabled != nil {
		globalCompression.headroomEnabled = *req.HeadroomEnabled
	}
	if req.HeadroomURL != nil {
		globalCompression.headroomURL = *req.HeadroomURL
	}
	if req.CavemanEnabled != nil {
		globalCompression.cavemanEnabled = *req.CavemanEnabled
	}
	if req.CavemanLevel != nil {
		globalCompression.cavemanLevel = *req.CavemanLevel
	}
	if req.PonytailEnabled != nil {
		globalCompression.ponytailEnabled = *req.PonytailEnabled
	}
	globalCompression.mu.Unlock()

	if req.ToolOutputEnabled != nil {
		config.SetBoolSetting("compressToolOutput", *req.ToolOutputEnabled)
	}
	if req.HeadroomEnabled != nil {
		config.SetBoolSetting("headroomEnabled", *req.HeadroomEnabled)
	}
	if req.HeadroomURL != nil {
		config.SetStringSetting("headroomURL", *req.HeadroomURL)
	}
	if req.CavemanEnabled != nil {
		config.SetBoolSetting("cavemanEnabled", *req.CavemanEnabled)
	}
	if req.CavemanLevel != nil {
		config.SetStringSetting("cavemanLevel", *req.CavemanLevel)
	}
	if req.PonytailEnabled != nil {
		config.SetBoolSetting("ponytailEnabled", *req.PonytailEnabled)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
