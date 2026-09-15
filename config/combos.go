package config

import (
	"errors"
	"reflect"
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
	cfg.ExtraModels = append([]string(nil), ids...)
	extraModelsExplicit = true
	return saveLocked()
}

// PublishDiscoveredModels reports whether the default /v1/models response
// includes the account-discovered model cache. See Config.PublishDiscoveredModels.
func PublishDiscoveredModels() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg != nil && cfg.PublishDiscoveredModels
}

// SetPublishDiscoveredModels toggles discovery publishing and persists it.
func SetPublishDiscoveredModels(on bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	cfg.PublishDiscoveredModels = on
	return saveLocked()
}

// GetAdaptiveRouting returns an isolated snapshot safe for request-time use.
// Maps and slices are copied because callers rank candidates outside cfgLock;
// returning aliases into cfg would race an admin/config reload.
func GetAdaptiveRouting() AdaptiveRoutingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return AdaptiveRoutingConfig{}
	}
	return cloneAdaptiveRouting(cfg.AdaptiveRouting)
}

// MatchAdaptiveRouting avoids cloning a potentially large policy for ordinary
// explicitly-named model requests. The snapshot is allocated only when the
// feature is enabled and requested matches its virtual model.
func MatchAdaptiveRouting(requested string) (AdaptiveRoutingConfig, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || !cfg.AdaptiveRouting.Enabled {
		return AdaptiveRoutingConfig{}, false
	}
	virtualModel := cfg.AdaptiveRouting.VirtualModel
	if virtualModel == "" {
		virtualModel = "omni-auto"
	}
	if requested != virtualModel {
		return AdaptiveRoutingConfig{}, false
	}
	return cloneAdaptiveRouting(cfg.AdaptiveRouting), true
}

func cloneAdaptiveRouting(in AdaptiveRoutingConfig) AdaptiveRoutingConfig {
	out := in
	out.DefaultRoute = append([]string(nil), in.DefaultRoute...)
	if in.Routes != nil {
		out.Routes = make(map[string][]string, len(in.Routes))
		for key, candidates := range in.Routes {
			out.Routes[key] = append([]string(nil), candidates...)
		}
	}
	if in.Profiles != nil {
		out.Profiles = make(map[string]AdaptiveModelProfile, len(in.Profiles))
		for model, profile := range in.Profiles {
			out.Profiles[model] = profile
		}
	}
	return out
}

var adaptiveRouteKeys = func() map[string]bool {
	keys := map[string]bool{"default": true}
	categories := []string{"general", "coding", "research", "creative", "vision", "default"}
	tiers := []string{"fast", "balanced", "strong", "default"}
	for _, category := range categories {
		for _, tier := range tiers {
			keys[category+"."+tier] = true
		}
	}
	return keys
}()

// normalizeAdaptiveRouting is the single policy boundary used by both startup
// loading and admin updates. It trims operator input, rejects unreachable route
// keys and recursion, and caps work performed on every adaptive request.
func normalizeAdaptiveRouting(in AdaptiveRoutingConfig) (AdaptiveRoutingConfig, bool, error) {
	original := cloneAdaptiveRouting(in)
	in.VirtualModel = strings.TrimSpace(in.VirtualModel)
	if in.VirtualModel == "" {
		in.VirtualModel = "omni-auto"
	}
	if strings.ContainsAny(in.VirtualModel, "/\r\n") {
		return AdaptiveRoutingConfig{}, false, errors.New("adaptive virtual model must not contain '/', CR, or LF")
	}
	in.Objective = strings.ToLower(strings.TrimSpace(in.Objective))
	if in.Objective == "" {
		in.Objective = "balanced"
	}
	switch in.Objective {
	case "balanced", "cost", "performance", "quality":
	default:
		return AdaptiveRoutingConfig{}, false, errors.New("adaptive objective must be balanced, cost, performance, or quality")
	}
	if len(in.Routes) > 64 || len(in.DefaultRoute) > 32 || len(in.Profiles) > 256 {
		return AdaptiveRoutingConfig{}, false, errors.New("adaptive routing policy exceeds size limits")
	}
	normalizeCandidates := func(label string, candidates []string) ([]string, error) {
		if len(candidates) > 32 {
			return nil, errors.New(label + " has more than 32 candidates")
		}
		out := make([]string, 0, len(candidates))
		seen := make(map[string]bool, len(candidates))
		for _, raw := range candidates {
			candidate := strings.TrimSpace(raw)
			if candidate == "" || strings.ContainsAny(candidate, "\r\n") {
				return nil, errors.New(label + " contains an empty or invalid candidate")
			}
			if candidate == in.VirtualModel {
				return nil, errors.New(label + " must not recursively target the adaptive virtual model")
			}
			if !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
		return out, nil
	}
	var err error
	if in.DefaultRoute, err = normalizeCandidates("defaultRoute", in.DefaultRoute); err != nil {
		return AdaptiveRoutingConfig{}, false, err
	}
	normalizedRoutes := make(map[string][]string, len(in.Routes))
	for rawKey, candidates := range in.Routes {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if !adaptiveRouteKeys[key] {
			return AdaptiveRoutingConfig{}, false, errors.New("unsupported adaptive route key: " + rawKey)
		}
		normalized, normalizeErr := normalizeCandidates("route "+key, candidates)
		if normalizeErr != nil {
			return AdaptiveRoutingConfig{}, false, normalizeErr
		}
		if _, duplicate := normalizedRoutes[key]; duplicate {
			return AdaptiveRoutingConfig{}, false, errors.New("duplicate adaptive route key after normalization: " + key)
		}
		normalizedRoutes[key] = normalized
	}
	if len(normalizedRoutes) == 0 {
		in.Routes = nil
	} else {
		in.Routes = normalizedRoutes
	}
	normalizedProfiles := make(map[string]AdaptiveModelProfile, len(in.Profiles))
	for rawModel, profile := range in.Profiles {
		model := strings.TrimSpace(rawModel)
		if model == "" || strings.ContainsAny(model, "\r\n") {
			return AdaptiveRoutingConfig{}, false, errors.New("adaptive profile model is empty or invalid")
		}
		// Zero means "use the family default"; non-zero overrides are bounded.
		if profile.Quality < 0 || profile.Quality > 100 {
			return AdaptiveRoutingConfig{}, false, errors.New("adaptive profile quality must be between 0 and 100")
		}
		if profile.LatencyMs < 0 || profile.LatencyMs > 3_600_000 {
			return AdaptiveRoutingConfig{}, false, errors.New("adaptive profile latencyMs must be between 0 and 3600000")
		}
		if _, duplicate := normalizedProfiles[model]; duplicate {
			return AdaptiveRoutingConfig{}, false, errors.New("duplicate adaptive profile after normalization: " + model)
		}
		normalizedProfiles[model] = profile
	}
	if len(normalizedProfiles) == 0 {
		in.Profiles = nil
	} else {
		in.Profiles = normalizedProfiles
	}
	changed := !reflect.DeepEqual(original, in)
	return in, changed, nil
}

// UpdateAdaptiveRouting validates and atomically persists the omni-auto policy.
func UpdateAdaptiveRouting(in AdaptiveRoutingConfig) error {
	normalized, _, err := normalizeAdaptiveRouting(in)
	if err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	previous := cfg.AdaptiveRouting
	cfg.AdaptiveRouting = cloneAdaptiveRouting(normalized)
	if err := saveLocked(); err != nil {
		cfg.AdaptiveRouting = previous
		return err
	}
	return nil
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
