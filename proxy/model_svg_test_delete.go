package proxy

import (
	"os"
	"path/filepath"
)

// deleteSVGTestEntry removes one stored (model, account, mode) result and
// reports whether the group has any entries left afterwards, so the caller can
// drop a group that became empty. svgEntryFileName sanitises the ids onto a safe
// stem, so the joined path cannot escape the group directory whatever the caller
// passes. An empty mode deletes the legacy (pre-mode) file for the pair, leaving
// raw/think results intact. A missing file is not an error here: removed=false
// lets the handler answer 404 without distinguishing "never stored" from
// "already gone".
func deleteSVGTestEntry(dir, promptKey, model, accountID, mode string) (removed, groupEmpty bool, err error) {
	if dir == "" || !validSVGPromptKey(promptKey) || model == "" || accountID == "" {
		return false, false, nil
	}
	path := filepath.Join(dir, promptKey, svgEntryFileName(model, accountID, mode))
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, len(getSVGTestGroupEntries(dir, promptKey)) == 0, nil
}
