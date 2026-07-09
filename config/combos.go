package config

import (
	"errors"
	"strings"
)

// ComboEntry represents a named sequential model fallback chain.
// When a request arrives with Model == combo.Name, the proxy tries
// each model in Models sequentially until one succeeds.
type ComboEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Models   []string `json:"models"`
	Strategy string   `json:"strategy,omitempty"` // "fallback" | "round-robin"; empty means use global default
}

// ListCombos returns a snapshot of all configured combos.
func ListCombos() []ComboEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ComboEntry, len(cfg.Combos))
	copy(out, cfg.Combos)
	return out
}

// GetComboByID returns a copy of the combo with the given ID, or nil if not found.
func GetComboByID(id string) *ComboEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.Combos {
		if cfg.Combos[i].ID == id {
			cp := cfg.Combos[i]
			return &cp
		}
	}
	return nil
}

// GetComboByName returns a copy of the combo with the given name, or nil if not found.
// This is the hot-path lookup used at request time: if the model string has no "/" and
// matches a combo name, it is dispatched as a combo.
func GetComboByName(name string) *ComboEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.Combos {
		if cfg.Combos[i].Name == name {
			cp := cfg.Combos[i]
			return &cp
		}
	}
	return nil
}

// AddCombo appends a new combo entry. It generates an ID if none is provided.
func AddCombo(entry ComboEntry) (ComboEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ComboEntry{}, errors.New("config not initialized")
	}
	entry.Name = strings.TrimSpace(entry.Name)
	if entry.Name == "" {
		return ComboEntry{}, errors.New("combo name must not be empty")
	}
	if strings.Contains(entry.Name, "/") {
		return ComboEntry{}, errors.New("combo name must not contain '/'")
	}
	if len(entry.Models) < 1 {
		return ComboEntry{}, errors.New("combo must have at least 1 model")
	}
	for _, existing := range cfg.Combos {
		if existing.Name == entry.Name {
			return ComboEntry{}, errors.New("combo name already exists")
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.Strategy != "round-robin" {
		entry.Strategy = "fallback"
	}
	cfg.Combos = append(cfg.Combos, entry)
	if err := saveLocked(); err != nil {
		cfg.Combos = cfg.Combos[:len(cfg.Combos)-1]
		return ComboEntry{}, err
	}
	return entry, nil
}

// ComboUpdateRequest holds the patchable fields for updating a combo.
type ComboUpdateRequest struct {
	Name     *string  `json:"name,omitempty"`
	Models   []string `json:"models,omitempty"`
	Strategy *string  `json:"strategy,omitempty"`
}

// UpdateCombo applies a partial update to the combo with the given ID.
func UpdateCombo(id string, patch ComboUpdateRequest) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.Combos {
		if cfg.Combos[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("combo not found")
	}
	if patch.Name != nil {
		newName := strings.TrimSpace(*patch.Name)
		if newName == "" {
			return errors.New("combo name must not be empty")
		}
		if strings.Contains(newName, "/") {
			return errors.New("combo name must not contain '/'")
		}
		for i, c := range cfg.Combos {
			if i != idx && c.Name == newName {
				return errors.New("combo name already exists")
			}
		}
		cfg.Combos[idx].Name = newName
	}
	if len(patch.Models) > 0 {
		if len(patch.Models) < 1 {
			return errors.New("combo must have at least 1 model")
		}
		cfg.Combos[idx].Models = patch.Models
	}
	if patch.Strategy != nil {
		if *patch.Strategy == "round-robin" {
			cfg.Combos[idx].Strategy = "round-robin"
		} else {
			cfg.Combos[idx].Strategy = "fallback"
		}
	}
	return saveLocked()
}

// DeleteCombo removes the combo with the given ID. Returns nil even if not found.
func DeleteCombo(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.Combos {
		if cfg.Combos[i].ID == id {
			cfg.Combos = append(cfg.Combos[:i], cfg.Combos[i+1:]...)
			return saveLocked()
		}
	}
	return nil
}

// GetExtraModels returns a snapshot of user-declared model IDs that should
// be advertised in /v1/models even when the upstream Kiro account doesn't
// list them. See Config.ExtraModels for the rationale.
func GetExtraModels() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]string, len(cfg.ExtraModels))
	copy(out, cfg.ExtraModels)
	return out
}

// SetExtraModels replaces the user-declared extra model list and persists it.
func SetExtraModels(ids []string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	cfg.ExtraModels = ids
	return saveLocked()
}

// GetComboStrategy returns the global default combo strategy ("fallback" or "round-robin").
func GetComboStrategy() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ComboStrategy == "" {
		return "fallback"
	}
	return cfg.ComboStrategy
}

// GetComboStickyRoundRobinLimit returns the global sticky round-robin limit (default 1).
func GetComboStickyRoundRobinLimit() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ComboStickyRoundRobinLimit <= 0 {
		return 1
	}
	return cfg.ComboStickyRoundRobinLimit
}

// UpdateComboSettings persists the global combo strategy settings.
func UpdateComboSettings(strategy string, stickyLimit int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if strategy != "round-robin" {
		strategy = "fallback"
	}
	if stickyLimit <= 0 {
		stickyLimit = 1
	}
	cfg.ComboStrategy = strategy
	cfg.ComboStickyRoundRobinLimit = stickyLimit
	return saveLocked()
}
