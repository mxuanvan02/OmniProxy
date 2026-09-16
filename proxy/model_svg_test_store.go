package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"omniproxy/config"
	"omniproxy/logger"
)

// svgTestEntry is one (model, account) outcome inside a prompt group. Results
// are stored one file per pair under data/svg-tests/<promptKey>/ so the same
// prompt can be rendered as a model x provider comparison grid.
type svgTestEntry struct {
	PromptKey   string `json:"promptKey"`
	Model       string `json:"model"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Provider    string `json:"provider"`
	Success     bool   `json:"success"`
	SVG         string `json:"svg"`
	Error       string `json:"error,omitempty"`
	ElapsedMs   int64  `json:"elapsedMs"`
	TokensUsed  int    `json:"tokensUsed"`
	SavedAt     int64  `json:"savedAt"`
}

// svgTestGroupMeta summarises one prompt without carrying SVG payloads.
// ResultCount, Models and Results are always derived from the directory scan on
// load, never persisted, so they cannot drift from the stored entries.
type svgTestGroupMeta struct {
	PromptKey   string           `json:"promptKey"`
	Prompt      string           `json:"prompt"`
	SavedAt     int64            `json:"savedAt"`
	ResultCount int              `json:"resultCount"`
	Models      []string         `json:"models"`
	Results     []svgTestSummary `json:"results"`
}

// svgTestSummary is the per-(model, account) row shown in the group list.
type svgTestSummary struct {
	Model       string `json:"model"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Provider    string `json:"provider"`
	Success     bool   `json:"success"`
	HasSVG      bool   `json:"hasSvg"`
}

// svgPromptKeyPattern keeps derived keys filesystem-safe: the key becomes a
// directory name, so anything beyond word characters and dashes is rejected.
// Legacy run ids written before prompt grouping match the same shape.
var svgPromptKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validSVGPromptKey(id string) bool { return svgPromptKeyPattern.MatchString(id) }

// svgPromptKey derives the group key from the prompt text itself, so the same
// prompt accumulates into one comparison no matter which client sent it or how
// many times it was run. Whitespace is normalised first so a trailing space
// does not fork a group.
func svgPromptKey(prompt string) string {
	normalized := strings.Join(strings.Fields(prompt), " ")
	sum := sha256.Sum256([]byte(normalized))
	return "p-" + hex.EncodeToString(sum[:])[:32]
}

// svgTestStoreDir returns the directory holding prompt groups, or "" when
// persistence is unavailable (config not initialised). Callers must treat ""
// as "skip storing", never as a path to write to.
func svgTestStoreDir() string {
	data := config.DataDir()
	if data == "" {
		return ""
	}
	return filepath.Join(data, "svg-tests")
}

// svgEntryFileName keys a stored result by model AND account. Keying by account
// alone collapsed two different models run against one account into a single
// file, losing the comparison the grid exists to show. Account ids are UUIDs, so
// the double-dash separator is unambiguous.
func svgEntryFileName(model, accountID string) string {
	return sanitizeIDForFile(model) + "--" + sanitizeIDForFile(accountID) + ".json"
}

// saveSVGTestEntry writes one (model, account) result into the prompt group.
// Failures are stored too: an account that errored is exactly the comparison
// data an operator wants beside the ones that succeeded.
func saveSVGTestEntry(dir string, entry svgTestEntry) error {
	if dir == "" || !validSVGPromptKey(entry.PromptKey) || entry.AccountID == "" {
		return nil
	}
	groupDir := filepath.Join(dir, entry.PromptKey)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(groupDir, svgEntryFileName(entry.Model, entry.AccountID)), data, 0o644)
}

// saveSVGTestMeta records the group's prompt text once. Every result rewrites it
// with the same value, so the last writer is representative.
func saveSVGTestMeta(dir, promptKey, prompt string) error {
	if dir == "" || !validSVGPromptKey(promptKey) {
		return nil
	}
	groupDir := filepath.Join(dir, promptKey)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return err
	}
	meta := svgTestGroupMeta{PromptKey: promptKey, Prompt: prompt, SavedAt: time.Now().Unix()}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(groupDir, "_meta.json"), data, 0o644)
}

// persistSVGTestResult stores one outcome and refreshes the group metadata. It
// is a no-op when persistence is unavailable, and its errors never fail the test
// itself: the SVG response is the primary output, the archive is a side effect.
func persistSVGTestResult(prompt, model string, entry svgTestEntry) {
	dir := svgTestStoreDir()
	prompt = strings.TrimSpace(prompt)
	if dir == "" || prompt == "" {
		return
	}
	key := svgPromptKey(prompt)
	entry.PromptKey = key
	entry.Model = model
	entry.SavedAt = time.Now().Unix()
	if err := saveSVGTestEntry(dir, entry); err != nil {
		logger.Errorf("[SVGTest] failed to save entry for prompt %s model %s account %s: %v", key, model, entry.AccountID, err)
		return
	}
	if err := saveSVGTestMeta(dir, key, prompt); err != nil {
		logger.Errorf("[SVGTest] failed to save meta for prompt %s: %v", key, err)
	}
}

// sanitizeIDForFile maps a model or account id onto a safe file stem.
func sanitizeIDForFile(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
