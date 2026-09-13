package proxy

import (
	"math"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const defaultAdaptiveVirtualModel = "omni-auto"

type adaptiveSignal struct {
	Text     string
	HasTools bool
	HasImage bool
	// CurrentChars drives complexity. InputChars drives cost estimation. Keeping
	// them separate prevents a long system prompt/history from upgrading "hi"
	// to a strong model while still pricing the full request honestly.
	CurrentChars int
	InputChars   int
}

type adaptiveDecision struct {
	Intercepted  bool
	VirtualModel string
	Category     string
	Tier         string
	RouteKey     string
	Objective    string
	Selected     string
	Recommended  string
	// Models is the concrete, de-duplicated execution chain. Named combos are
	// flattened after candidate groups are ranked by the adaptive objective.
	Models []string
	Shadow bool
	Reason string
}

type adaptiveCandidate struct {
	Name      string
	Model     string
	Available bool
	Quality   float64
	LatencyMs float64
	CostUSD   float64
	CostKnown bool
	Order     int
	Score     float64
}

var (
	adaptiveGreetingRE = regexp.MustCompile(`(?i)^\s*(hi|hello|hey|yo|thanks|thank you|ok|okay|yes|no|bye|xin chào|chào|cảm ơn|được|ừ)\s*[!.?]*\s*$`)
	adaptiveContinueRE = regexp.MustCompile(`(?i)^\s*(continue|go on|keep going|more|again|tiếp|tiếp tục|làm tiếp|nữa|thêm)\b`)
	adaptiveCodingRE   = regexp.MustCompile(`(?i)\b(code|coding|debug|bug|error|exception|traceback|refactor|implement|function|class|api|database|sql|typescript|javascript|python|golang|rust|docker|kubernetes|test|build|compile|repo|repository|source|mã nguồn|lập trình|sửa lỗi|triển khai|kiểm thử)\b`)
	adaptiveResearchRE = regexp.MustCompile(`(?i)\b(research|compare|analy[sz]e|evaluate|evidence|source|citation|study|investigate|nghiên cứu|so sánh|phân tích|đánh giá|nguồn|bằng chứng|kiểm chứng)\b`)
	adaptiveCreativeRE = regexp.MustCompile(`(?i)\b(write|draft|story|poem|creative|screenplay|brand voice|article|essay|viết|soạn|truyện|thơ|sáng tạo|kịch bản|bài viết)\b`)
	adaptiveDeepRE     = regexp.MustCompile(`(?i)\b(architect|architecture|audit|migrate|integrate|benchmark|root cause|end[- ]to[- ]end|comprehensive|thorough|production|security|performance|optimi[sz]e|kiến trúc|toàn diện|chuyên sâu|hiệu năng|bảo mật|tối ưu)\b`)
	adaptiveStepRE     = regexp.MustCompile(`(?i)\b(and|then|also|plus|with|và|sau đó|đồng thời|kèm theo)\b`)
)

func adaptiveVirtualModel(cfg config.AdaptiveRoutingConfig) string {
	if model := strings.TrimSpace(cfg.VirtualModel); model != "" {
		return model
	}
	return defaultAdaptiveVirtualModel
}

func adaptiveSignalFromOpenAI(req *OpenAIRequest) adaptiveSignal {
	if req == nil {
		return adaptiveSignal{}
	}
	sig := adaptiveSignal{InputChars: estimateOpenAIRequestInputTokens(req) * 4}
	userTurns := make([]string, 0, 4)
	for i, msg := range req.Messages {
		// Declared tools are capability metadata, not evidence that this turn is
		// an agent/tool task. Only an active final tool result upgrades the route.
		if i == len(req.Messages)-1 && (msg.Role == "tool" || msg.ToolCallID != "") {
			sig.HasTools = true
		}
		if msg.Role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		// Vision is current-turn intent. Historical images must not force every
		// later text-only follow-up onto a vision route.
		sig.HasImage = len(images) > 0
		if strings.TrimSpace(text) != "" {
			userTurns = append(userTurns, text)
		}
	}
	sig.Text = adaptiveIntentText(userTurns)
	sig.CurrentChars = len(sig.Text)
	return sig
}

func adaptiveSignalFromClaude(req *ClaudeRequest) adaptiveSignal {
	if req == nil {
		return adaptiveSignal{}
	}
	sig := adaptiveSignal{InputChars: estimateClaudeRequestInputTokens(req) * 4}
	userTurns := make([]string, 0, 4)
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		text, images, toolResults := extractClaudeUserContent(msg.Content)
		sig.HasImage = len(images) > 0
		sig.HasTools = len(toolResults) > 0
		if strings.TrimSpace(text) != "" {
			userTurns = append(userTurns, text)
		}
	}
	sig.Text = adaptiveIntentText(userTurns)
	sig.CurrentChars = len(sig.Text)
	return sig
}

func adaptiveIntentText(userTurns []string) string {
	if len(userTurns) == 0 {
		return ""
	}
	current := strings.TrimSpace(userTurns[len(userTurns)-1])
	// A tiny continuation inherits the preceding substantive task. Routing the
	// word "continue" as a cheap greeting loses quality on long agent sessions.
	if len([]rune(current)) <= 48 && adaptiveContinueRE.MatchString(current) {
		for i := len(userTurns) - 2; i >= 0; i-- {
			if prior := strings.TrimSpace(userTurns[i]); prior != "" && !adaptiveContinueRE.MatchString(prior) {
				return prior
			}
		}
	}
	return current
}

func classifyAdaptiveSignal(sig adaptiveSignal) (category, tier, reason string) {
	text := strings.TrimSpace(sig.Text)
	switch {
	case sig.HasImage:
		category = "vision"
		reason = "image input"
	case sig.HasTools || adaptiveCodingRE.MatchString(text):
		category = "coding"
		if sig.HasTools {
			reason = "tool context"
		} else {
			reason = "coding signals"
		}
	case adaptiveResearchRE.MatchString(text):
		category, reason = "research", "research signals"
	case adaptiveCreativeRE.MatchString(text):
		category, reason = "creative", "creative signals"
	default:
		category, reason = "general", "default category"
	}

	words := len(strings.Fields(text))
	deepSignals := len(adaptiveDeepRE.FindAllString(text, -1))
	steps := len(adaptiveStepRE.FindAllString(text, -1))
	switch {
	case adaptiveGreetingRE.MatchString(text) && !sig.HasTools && !sig.HasImage:
		tier = "fast"
	case sig.HasTools || sig.CurrentChars >= 6000 || words >= 220 || deepSignals >= 2 || (deepSignals >= 1 && steps >= 2):
		tier = "strong"
	default:
		tier = "balanced"
	}
	return category, tier, reason
}

func normalizeAdaptiveObjective(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cost", "performance", "quality", "balanced":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "balanced"
	}
}

func adaptiveRouteCandidates(cfg config.AdaptiveRoutingConfig, category, tier string) ([]string, string) {
	keys := []string{category + "." + tier, category + ".default", "default." + tier, "default"}
	for _, key := range keys {
		if candidates := cleanAdaptiveCandidates(cfg.Routes[key]); len(candidates) > 0 {
			return candidates, key
		}
	}
	return cleanAdaptiveCandidates(cfg.DefaultRoute), "defaultRoute"
}

func cleanAdaptiveCandidates(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (h *Handler) resolveAdaptiveModel(requested string, sig adaptiveSignal) adaptiveDecision {
	cfg, matched := config.MatchAdaptiveRouting(requested)
	decision := adaptiveDecision{}
	if !matched {
		return decision
	}
	decision.Intercepted = true
	decision.VirtualModel = adaptiveVirtualModel(cfg)
	decision.Objective = normalizeAdaptiveObjective(cfg.Objective)
	decision.Category, decision.Tier, decision.Reason = classifyAdaptiveSignal(sig)
	candidates, routeKey := adaptiveRouteCandidates(cfg, decision.Category, decision.Tier)
	decision.RouteKey = routeKey

	recommended := h.rankAdaptiveCandidates(candidates, decision.Tier, decision.Objective, cfg.Profiles, sig)
	if len(recommended) > 0 {
		decision.Recommended = recommended[0].Name
	}
	selected := recommended
	decision.Shadow = cfg.ShadowMode
	if cfg.ShadowMode {
		selected = h.rankAdaptiveCandidates(cleanAdaptiveCandidates(cfg.DefaultRoute), decision.Tier, decision.Objective, cfg.Profiles, sig)
	}
	decision.Models = h.expandAdaptiveCandidates(selected)
	if len(decision.Models) > 0 {
		decision.Selected = decision.Models[0]
		logger.Infof("[ADAPTIVE] requested=%s category=%s tier=%s route=%s objective=%s selected=%s recommended=%s fallback_models=%d shadow=%t reason=%s",
			requested, decision.Category, decision.Tier, decision.RouteKey, decision.Objective,
			decision.Selected, decision.Recommended, len(decision.Models), decision.Shadow, decision.Reason)
	} else {
		logger.Warnf("[ADAPTIVE] requested=%s category=%s tier=%s route=%s has no viable candidate", requested, decision.Category, decision.Tier, decision.RouteKey)
	}
	return decision
}

func (h *Handler) rankAdaptiveCandidates(names []string, tier, objective string, profiles map[string]config.AdaptiveModelProfile, sig adaptiveSignal) []adaptiveCandidate {
	candidates := make([]adaptiveCandidate, 0, len(names))
	for order, name := range names {
		model, available, ok := h.adaptiveRepresentativeModel(name, make(map[string]bool), 0)
		if !ok {
			continue
		}
		profile, exists := profiles[name]
		if !exists {
			profile = profiles[model]
		}
		quality, latency := adaptiveModelDefaults(model)
		if profile.Quality > 0 {
			quality = profile.Quality
		}
		if profile.LatencyMs > 0 {
			latency = float64(profile.LatencyMs)
		}
		cost, known := adaptiveEstimatedCost(model, sig, tier)
		candidates = append(candidates, adaptiveCandidate{
			Name: name, Model: model, Available: available, Quality: quality, LatencyMs: latency,
			CostUSD: cost, CostKnown: known, Order: order,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	// A cost objective may economize within an adequate capability band, but it
	// must not buy a weak primary for a strong task merely because it is cheap.
	// Below-floor candidates are retained as degraded fallbacks.
	floor := map[string]float64{"fast": 45, "balanced": 60, "strong": 75}[tier]
	fillUnknownAdaptiveCosts(candidates)
	minQ, maxQ := adaptiveRange(candidates, func(c adaptiveCandidate) float64 { return c.Quality })
	minL, maxL := adaptiveRange(candidates, func(c adaptiveCandidate) float64 { return c.LatencyMs })
	minC, maxC := adaptiveRange(candidates, func(c adaptiveCandidate) float64 { return c.CostUSD })
	qw, lw, cw := adaptiveWeights(objective, tier)
	for i := range candidates {
		q := adaptiveNormalize(candidates[i].Quality, minQ, maxQ, true)
		l := adaptiveNormalize(candidates[i].LatencyMs, minL, maxL, false)
		c := adaptiveNormalize(candidates[i].CostUSD, minC, maxC, false)
		candidates[i].Score = qw*q + lw*l + cw*c
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// Prefer capacity that can serve immediately, but retain temporarily
		// unavailable supported models so the normal pool-recovery path can wait.
		if candidates[i].Available != candidates[j].Available {
			return candidates[i].Available
		}
		iAdequate := candidates[i].Quality >= floor
		jAdequate := candidates[j].Quality >= floor
		if iAdequate != jAdequate {
			return iAdequate
		}
		if math.Abs(candidates[i].Score-candidates[j].Score) < 1e-9 {
			return candidates[i].Order < candidates[j].Order
		}
		return candidates[i].Score > candidates[j].Score
	})
	return candidates
}

// expandAdaptiveCandidates flattens ranked combo groups into a concrete model
// fallback chain. Supported models are retained even during a transient
// cooldown; the execution path owns waiting/recovery. Duplicates are removed.
func (h *Handler) expandAdaptiveCandidates(candidates []adaptiveCandidate) []string {
	models := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	var expand func(string, map[string]bool, int)
	expand = func(name string, visiting map[string]bool, depth int) {
		if depth > 8 || visiting[name] {
			return
		}
		if combo := config.GetComboByName(name); combo != nil {
			visiting[name] = true
			for _, child := range combo.Models {
				expand(child, visiting, depth+1)
			}
			delete(visiting, name)
			return
		}
		model, _ := ParseModelAndThinking(name, config.GetThinkingConfig().Suffix)
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" || seen[key] || h == nil || h.pool == nil || h.pool.CountAccountsForModel(model) == 0 {
			return
		}
		seen[key] = true
		models = append(models, model)
	}
	for _, candidate := range candidates {
		expand(candidate.Name, make(map[string]bool), 0)
	}
	return models
}

func (h *Handler) adaptiveRepresentativeModel(name string, visiting map[string]bool, depth int) (string, bool, bool) {
	if h == nil || h.pool == nil || depth > 8 || visiting[name] {
		return "", false, false
	}
	if combo := config.GetComboByName(name); combo != nil {
		visiting[name] = true
		defer delete(visiting, name)
		var firstSupported string
		for _, child := range combo.Models {
			model, available, ok := h.adaptiveRepresentativeModel(child, visiting, depth+1)
			if !ok {
				continue
			}
			if firstSupported == "" {
				firstSupported = model
			}
			if available {
				return model, true, true
			}
		}
		if firstSupported != "" {
			return firstSupported, false, true
		}
		return "", false, false
	}
	model, _ := ParseModelAndThinking(name, config.GetThinkingConfig().Suffix)
	if h.pool.CountAccountsForModel(model) == 0 {
		return "", false, false
	}
	return model, h.pool.HasAvailableAccountForModel(model), true
}

func adaptiveModelDefaults(model string) (quality, latencyMs float64) {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "sol"), strings.Contains(lower, "opus"), strings.Contains(lower, "fable"), strings.Contains(lower, "mythos"), strings.Contains(lower, "model-s"):
		return 94, 5500
	case strings.Contains(lower, "terra"), strings.Contains(lower, "sonnet"), strings.Contains(lower, "model-t"), strings.Contains(lower, "model-o"):
		return 84, 3000
	case strings.Contains(lower, "luna"), strings.Contains(lower, "haiku"), strings.Contains(lower, "flash"), strings.Contains(lower, "mini"):
		return 64, 1200
	default:
		return 72, 2500
	}
}

func adaptiveEstimatedCost(model string, sig adaptiveSignal, tier string) (float64, bool) {
	pricing, ok := LookupPricing(model)
	if !ok {
		return 0, false
	}
	input := sig.InputChars / 4
	if input < 1 {
		input = 1
	}
	output := map[string]int{"fast": 256, "balanced": 1024, "strong": 2048}[tier]
	if pricing.PerCallUSD > 0 {
		return pricing.PerCallUSD, true
	}
	return (float64(input)*pricing.InputPerM + float64(output)*pricing.OutputPerM) / 1_000_000, true
}

func fillUnknownAdaptiveCosts(candidates []adaptiveCandidate) {
	maxKnown := 0.0
	for _, candidate := range candidates {
		if candidate.CostKnown && candidate.CostUSD > maxKnown {
			maxKnown = candidate.CostUSD
		}
	}
	if maxKnown == 0 {
		maxKnown = 1
	}
	for i := range candidates {
		if !candidates[i].CostKnown {
			candidates[i].CostUSD = maxKnown * 1.25
		}
	}
}

func adaptiveWeights(objective, tier string) (quality, latency, cost float64) {
	switch objective {
	case "cost":
		return .15, .10, .75
	case "performance":
		return .15, .75, .10
	case "quality":
		return .80, .10, .10
	default:
		switch tier {
		case "fast":
			return .20, .40, .40
		case "strong":
			return .70, .15, .15
		default:
			return .40, .30, .30
		}
	}
}

func adaptiveRange(candidates []adaptiveCandidate, value func(adaptiveCandidate) float64) (float64, float64) {
	min, max := value(candidates[0]), value(candidates[0])
	for _, candidate := range candidates[1:] {
		v := value(candidate)
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func adaptiveNormalize(value, min, max float64, higherBetter bool) float64 {
	if max <= min {
		return 1
	}
	n := (value - min) / (max - min)
	if !higherBetter {
		return 1 - n
	}
	return n
}

// applyAdaptiveTraceHeaders exposes only routing metadata, never prompt text.
// Header values are sanitized because route/model names are operator input.
func applyAdaptiveTraceHeaders(w http.ResponseWriter, decision adaptiveDecision) {
	if w == nil || !decision.Intercepted {
		return
	}
	set := func(name, value string) {
		value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
		if len(value) > 256 {
			value = value[:256]
		}
		if value != "" {
			w.Header().Set(name, value)
		}
	}
	set("X-Omni-Adaptive", "1")
	set("X-Omni-Route-Category", decision.Category)
	set("X-Omni-Route-Tier", decision.Tier)
	set("X-Omni-Route-Key", decision.RouteKey)
	set("X-Omni-Route-Objective", decision.Objective)
	set("X-Omni-Route-Selected", decision.Selected)
	set("X-Omni-Route-Recommended", decision.Recommended)
	set("X-Omni-Route-Shadow", strconv.FormatBool(decision.Shadow))
}

func adaptiveUnavailableMessage(decision adaptiveDecision) string {
	return "adaptive route unavailable for " + decision.Category + "." + decision.Tier
}

// adaptiveCatalogEntry advertises the virtual model only while it is active.
// A disabled feature therefore cannot be selected accidentally by a client
// that merely cached /v1/models from a previous deployment.
func adaptiveCatalogEntry() map[string]interface{} {
	cfg := config.GetAdaptiveRouting()
	if !cfg.Enabled {
		return nil
	}
	entry := buildModelInfo(adaptiveVirtualModel(cfg), "omniproxy", true)
	entry["adaptive"] = true
	entry["description"] = "Deterministic cost/performance/quality adaptive router"
	return entry
}
