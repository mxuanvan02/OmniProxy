package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// listSVGTestGroups returns one summary per prompt group, newest first, capped
// at limit. A group is identified by its prompt, so repeated comparisons of the
// same prompt against different models accumulate into one entry.
func listSVGTestGroups(dir string, limit int) []svgTestGroupMeta {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]svgTestGroupMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !validSVGPromptKey(e.Name()) {
			continue
		}
		if meta := loadSVGTestGroup(filepath.Join(dir, e.Name()), e.Name()); meta != nil {
			out = append(out, *meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// loadSVGTestGroup reads one group's prompt and rebuilds the per-result summary
// list by scanning the entry files, so counts and model lists cannot drift.
func loadSVGTestGroup(groupDir, promptKey string) *svgTestGroupMeta {
	meta := &svgTestGroupMeta{PromptKey: promptKey}
	if raw, err := os.ReadFile(filepath.Join(groupDir, "_meta.json")); err == nil {
		_ = json.Unmarshal(raw, meta)
		meta.PromptKey = promptKey
	}
	meta.ResultCount = 0
	meta.Results = nil
	modelSet := map[string]bool{}
	files, err := os.ReadDir(groupDir)
	if err != nil {
		return nil
	}
	for _, f := range files {
		entry := readSVGTestEntry(filepath.Join(groupDir, f.Name()))
		if entry == nil {
			continue
		}
		if entry.SavedAt > meta.SavedAt {
			meta.SavedAt = entry.SavedAt
		}
		if entry.Model != "" {
			modelSet[entry.Model] = true
		}
		meta.ResultCount++
		meta.Results = append(meta.Results, svgTestSummary{
			Model: entry.Model, AccountID: entry.AccountID, AccountName: entry.AccountName,
			Provider: entry.Provider, Success: entry.Success, HasSVG: entry.SVG != "",
		})
	}
	if meta.ResultCount == 0 {
		return nil
	}
	meta.Models = make([]string, 0, len(modelSet))
	for m := range modelSet {
		meta.Models = append(meta.Models, m)
	}
	sort.Strings(meta.Models)
	sort.Slice(meta.Results, func(i, j int) bool {
		if meta.Results[i].Model != meta.Results[j].Model {
			return meta.Results[i].Model < meta.Results[j].Model
		}
		return meta.Results[i].AccountName < meta.Results[j].AccountName
	})
	return meta
}

// getSVGTestGroupEntries returns the full entries (SVG included) of one group,
// ordered by model then account name for stable grid rendering.
func getSVGTestGroupEntries(dir, promptKey string) []svgTestEntry {
	if dir == "" || !validSVGPromptKey(promptKey) {
		return nil
	}
	files, err := os.ReadDir(filepath.Join(dir, promptKey))
	if err != nil {
		return nil
	}
	out := []svgTestEntry{}
	for _, f := range files {
		if entry := readSVGTestEntry(filepath.Join(dir, promptKey, f.Name())); entry != nil {
			out = append(out, *entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].AccountName < out[j].AccountName
	})
	return out
}

// readSVGTestEntry loads one entry file, returning nil for the metadata file,
// unreadable files, and anything that is not a valid entry.
func readSVGTestEntry(path string) *svgTestEntry {
	if strings.HasSuffix(path, "_meta.json") || !strings.HasSuffix(path, ".json") {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entry svgTestEntry
	if json.Unmarshal(raw, &entry) != nil {
		return nil
	}
	return &entry
}
