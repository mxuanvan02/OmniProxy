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
			Provider: entry.Provider, Dialect: entry.Dialect, Mode: entry.Mode, Effort: entry.Effort,
			Success: entry.Success, HasSVG: entry.SVG != "", Score: entry.Score,
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
		return svgTestResultLess(
			meta.Results[i].Model, meta.Results[i].AccountName, meta.Results[i].Mode, meta.Results[i].Effort,
			meta.Results[j].Model, meta.Results[j].AccountName, meta.Results[j].Mode, meta.Results[j].Effort,
		)
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
		return svgTestResultLess(out[i].Model, out[i].AccountName, out[i].Mode, out[i].Effort, out[j].Model, out[j].AccountName, out[j].Mode, out[j].Effort)
	})
	return out
}

// svgTestResultLess orders results the way an operator reads them: grouped by
// model, then by account, then by mode, then by reasoning effort. Putting
// effort last keeps a sweep's low-to-high rungs adjacent inside one cell.
func svgTestResultLess(modelA, accountA, modeA, effortA, modelB, accountB, modeB, effortB string) bool {
	if modelA != modelB {
		return modelA < modelB
	}
	if accountA != accountB {
		return accountA < accountB
	}
	if modeA != modeB {
		return modeA < modeB
	}
	return svgTestEffortRank(effortA) < svgTestEffortRank(effortB)
}

// svgTestEffortRank orders effort by increasing spend so a sweep curve is
// already sorted low→high. Unspecified and unrecognised values rank first;
// "max" is the top rung.
func svgTestEffortRank(effort string) int {
	switch normalizeSVGTestEffort(effort) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "max":
		return 4
	default:
		return 0
	}
}

// readSVGTestEntry loads one entry file, returning nil for the metadata file,
// unreadable files, and anything that is not a valid entry. The score is
// recomputed here rather than read from disk: this is the single funnel both
// readers go through, so legacy files written before scoring existed still get
// a grade and a rubric change re-grades everything instead of leaving stale
// numbers in the archive.
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
	if entry.Success {
		entry.Score, entry.ScoreReasons = scoreSVG(entry.SVG)
	} else {
		entry.Score, entry.ScoreReasons = 0, nil
	}
	return &entry
}
