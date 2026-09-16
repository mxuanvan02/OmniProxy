package proxy

import (
	"os"
	"path/filepath"
)

// deleteSVGTestEntry removes one stored (model, account) result and reports
// whether the group has any entries left afterwards, so the caller can drop a
// group that became empty. svgEntryFileName sanitises both ids onto a safe
// stem, so the joined path cannot escape the group directory whatever the
// caller passes. A missing file is not an error here: removed=false lets the
// handler answer 404 without distinguishing "never stored" from "already gone".
func deleteSVGTestEntry(dir, promptKey, model, accountID string) (removed, groupEmpty bool, err error) {
	if dir == "" || !validSVGPromptKey(promptKey) || model == "" || accountID == "" {
		return false, false, nil
	}
	path := filepath.Join(dir, promptKey, svgEntryFileName(model, accountID))
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, len(getSVGTestGroupEntries(dir, promptKey)) == 0, nil
}
