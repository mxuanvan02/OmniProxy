package proxy

import (
	"encoding/json"
	"omniproxy/config"
	"omniproxy/logger"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Endpoint labels identify which client-facing API a request came through. They
// are the "endpoint" dimension in usage accounting (By Endpoint table) and the
// in-flight label in TrackActive — distinct from the upstream account.Provider
// (BuilderId/ExternalIdp) that the "provider" dimension records. Use these
// constants everywhere instead of bare string literals so the two roles never
// get crossed again (e.g. an OpenAI request mislabelled "claude").
const (
	endpointClaude          = "claude"
	endpointOpenAI          = "openai"
	endpointOpenAIResponses = "openai-responses"
)

// Request status values stored on RequestRecord.Status.
const (
	statusSuccess = "success"
	statusError   = "error"
)

// RequestRecord is a single usage event captured during a proxy request.
type RequestRecord struct {
	Timestamp                  string  `json:"timestamp"`
	Model                      string  `json:"model"`
	Provider                   string  `json:"provider"`
	AccountID                  string  `json:"accountId"`
	AccountName                string  `json:"accountName"`
	InputTokens                int     `json:"inputTokens"`
	OutputTokens               int     `json:"outputTokens"`
	Cost                       float64 `json:"cost"`                      // upstream-reported credits (legacy)
	RealCost                   float64 `json:"realCost,omitempty"`        // USD computed from model pricing
	InputCost                  float64 `json:"inputCost,omitempty"`       // USD for uncached input tokens
	CachedCost                 float64 `json:"cachedCost,omitempty"`      // USD for cached input tokens (cache-read rate)
	OutputCost                 float64 `json:"outputCost,omitempty"`      // USD for output tokens
	EffectiveTokens            int     `json:"effectiveTokens,omitempty"` // (input - cached) + output
	Status                     string  `json:"status"`
	Endpoint                   string  `json:"endpoint"`
	APIKeyID                   string  `json:"apiKeyId,omitempty"`
	Error                      string  `json:"error,omitempty"`
	CacheReadTokens            int     `json:"cacheReadTokens,omitempty"`
	CacheCreateTokens          int     `json:"cacheCreateTokens,omitempty"`
	CachedTokens               int     `json:"cachedTokens,omitempty"` // OpenAI-style cached prompt tokens
	CacheSource                string  `json:"cacheSource,omitempty"`  // upstream, estimated, or none
	EstimatedCacheReadTokens   int     `json:"estimatedCacheReadTokens,omitempty"`
	EstimatedCacheCreateTokens int     `json:"estimatedCacheCreateTokens,omitempty"`
}

// PeriodSummary holds aggregated stats for a single time bucket.
//
// The By* breakdown maps are populated only on the per-day daily buckets so the
// usage breakdowns survive past the ring buffer's 500-record cap. They are
// omitempty + nil on legacy daily files and on the nested per-dimension
// summaries (which never carry their own sub-breakdowns).
type PeriodSummary struct {
	Requests          int     `json:"requests"`
	PromptTokens      int     `json:"promptTokens"`
	CompletionTokens  int     `json:"completionTokens"`
	Cost              float64 `json:"cost"`                      // legacy upstream-reported credits
	RealCost          float64 `json:"realCost,omitempty"`        // USD from ComputeCost
	InputCost         float64 `json:"inputCost,omitempty"`       // USD for uncached input
	CachedCost        float64 `json:"cachedCost,omitempty"`      // USD for cached input
	OutputCost        float64 `json:"outputCost,omitempty"`      // USD for output
	EffectiveTokens   int     `json:"effectiveTokens,omitempty"` // (input - cached) + output
	CacheReadTokens   int     `json:"cacheReadTokens,omitempty"`
	CacheCreateTokens int     `json:"cacheCreateTokens,omitempty"`
	CachedTokens      int     `json:"cachedTokens,omitempty"`
	// UpstreamCache* only contains source-tagged records written after cache
	// telemetry was made authoritative. Legacy cache counters may include local
	// predictions and remain available only for backwards-compatible history.
	UpstreamCacheReadTokens    int `json:"upstreamCacheReadTokens,omitempty"`
	UpstreamCacheCreateTokens  int `json:"upstreamCacheCreateTokens,omitempty"`
	UpstreamCachedTokens       int `json:"upstreamCachedTokens,omitempty"`
	EstimatedCacheReadTokens   int `json:"estimatedCacheReadTokens,omitempty"`
	EstimatedCacheCreateTokens int `json:"estimatedCacheCreateTokens,omitempty"`

	ByModel    map[string]*PeriodSummary `json:"byModel,omitempty"`
	ByAccount  map[string]*PeriodSummary `json:"byAccount,omitempty"`
	ByAPIKey   map[string]*PeriodSummary `json:"byApiKey,omitempty"`
	ByEndpoint map[string]*PeriodSummary `json:"byEndpoint,omitempty"`
}

// UsageStats holds the full response for the usage stats endpoint.
type UsageStats struct {
	TotalRequests                   int                       `json:"totalRequests"`
	TotalPromptTokens               int                       `json:"totalPromptTokens"`
	TotalCompletionTokens           int                       `json:"totalCompletionTokens"`
	TotalCost                       float64                   `json:"totalCost"`                      // legacy credits
	TotalRealCost                   float64                   `json:"totalRealCost,omitempty"`        // USD from pricing
	TotalInputCost                  float64                   `json:"totalInputCost,omitempty"`       // USD uncached input
	TotalCachedCost                 float64                   `json:"totalCachedCost,omitempty"`      // USD cached input
	TotalOutputCost                 float64                   `json:"totalOutputCost,omitempty"`      // USD output
	TotalEffectiveTokens            int                       `json:"totalEffectiveTokens,omitempty"` // (input-cached)+output
	TotalCacheReadTokens            int                       `json:"totalCacheReadTokens,omitempty"`
	TotalCacheCreateTokens          int                       `json:"totalCacheCreateTokens,omitempty"`
	TotalCachedTokens               int                       `json:"totalCachedTokens,omitempty"`
	TotalUpstreamCacheReadTokens    int                       `json:"totalUpstreamCacheReadTokens,omitempty"`
	TotalUpstreamCacheCreateTokens  int                       `json:"totalUpstreamCacheCreateTokens,omitempty"`
	TotalUpstreamCachedTokens       int                       `json:"totalUpstreamCachedTokens,omitempty"`
	TotalEstimatedCacheReadTokens   int                       `json:"totalEstimatedCacheReadTokens,omitempty"`
	TotalEstimatedCacheCreateTokens int                       `json:"totalEstimatedCacheCreateTokens,omitempty"`
	ActiveRequests                  []ActiveRequest           `json:"activeRequests"`
	RecentRequests                  []RequestRecord           `json:"recentRequests"`
	ByModel                         map[string]*PeriodSummary `json:"byModel"`
	ByAccount                       map[string]*PeriodSummary `json:"byAccount"`
	ByAPIKey                        map[string]*PeriodSummary `json:"byApiKey"`
	ByEndpoint                      map[string]*PeriodSummary `json:"byEndpoint"`
	ErrorProvider                   string                    `json:"errorProvider"`
	AccountNames                    map[string]string         `json:"accountNames"`
}

// cacheHitTokens returns the number of prompt tokens served from cache. When
// effective tokens are available, they retain the per-request cache clamping
// applied at ingestion and are therefore the authoritative aggregate source.
// Legacy aggregates without effective tokens fall back to their two cache
// counters and remain bounded by input tokens.
func cacheHitTokens(input, output, effective, cacheRead, cachedTokens int) int {
	if effective > 0 {
		return clampInt(input+output-effective, 0, input)
	}
	return clampInt(cacheRead+cachedTokens, 0, input)
}

// ActiveRequest represents an in-flight request for the topology.
type ActiveRequest struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	AccountID string `json:"accountId"`
}

// ChartDataPoint is a single bucket in the time-series chart.
type ChartDataPoint struct {
	Label  string  `json:"label"`
	Tokens int     `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// UsageTracker collects per-request usage data in memory.
type UsageTracker struct {
	mu          sync.RWMutex
	ring        []RequestRecord
	ringCap     int
	ringIdx     int
	ringFull    bool
	activeReqs  map[string]ActiveRequest // accountID → request
	dailyData   map[string]*PeriodSummary
	dirty       bool
	historyPath string
	dailyPath   string
	eventSeq    uint64 // monotonic process-local sequence for live request events
}

var globalTracker *UsageTracker
var trackerOnce sync.Once

func GetUsageTracker() *UsageTracker {
	trackerOnce.Do(func() {
		dataDir := config.GetConfigDir()
		globalTracker = &UsageTracker{
			ringCap:     500,
			ring:        make([]RequestRecord, 500),
			activeReqs:  make(map[string]ActiveRequest),
			dailyData:   make(map[string]*PeriodSummary),
			historyPath: filepath.Join(dataDir, "usage_history.json"),
			dailyPath:   filepath.Join(dataDir, "usage_daily.json"),
		}
		globalTracker.loadFromDisk()
		// Periodically flush to disk
		go globalTracker.periodicFlush()
	})
	return globalTracker
}

func (t *UsageTracker) loadFromDisk() {
	// Load ring history
	if data, err := os.ReadFile(t.historyPath); err == nil {
		var records []RequestRecord
		if json.Unmarshal(data, &records) == nil {
			for _, r := range records {
				// Backfill realCost + cost breakdown + effectiveTokens on legacy
				// ring records that were persisted before these fields existed.
				cached := r.CacheReadTokens
				if r.CachedTokens > cached {
					cached = r.CachedTokens
				}
				if r.EffectiveTokens == 0 {
					r.EffectiveTokens = EffectiveTokens(r.InputTokens, cached, r.OutputTokens)
				}
				if r.RealCost == 0 {
					bd := ComputeCostBreakdown(r.Model, r.InputTokens, cached, r.OutputTokens)
					r.RealCost = bd.Total
					r.InputCost = bd.InputCost
					r.CachedCost = bd.CachedCost
					r.OutputCost = bd.OutputCost
				}
				t.pushToRing(r)
			}
		}
	}
	// Load daily aggregations
	if data, err := os.ReadFile(t.dailyPath); err == nil {
		json.Unmarshal(data, &t.dailyData)
		// Backfill RealCost/EffectiveTokens on legacy daily buckets whose
		// ByModel breakdowns have token counts but no computed cost (written
		// before the pricing module existed). Headline day totals are left
		// as-is — they will be re-derived from ByModel on the next read.
		for _, day := range t.dailyData {
			backfillDaySummary(day)
		}
	}
}

// backfillDaySummary recomputes RealCost + EffectiveTokens + cost breakdown for
// a legacy daily bucket that was persisted without these fields. Iterates the
// ByModel map (which has per-model token counts) and sums up. Also fixes the
// day-level headline totals so they match the breakdown.
func backfillDaySummary(day *PeriodSummary) {
	if day == nil || day.ByModel == nil {
		return
	}
	// Skip only if the breakdown is already complete (all 3 cost fields + realCost).
	// Older versions wrote RealCost without InputCost/CachedCost/OutputCost, so
	// we still need to backfill the breakdown in that case.
	if day.RealCost > 0 && day.InputCost > 0 && day.CachedCost > 0 && day.OutputCost > 0 {
		return
	}
	// Reset day-level cost fields before re-summing from ByModel to avoid
	// double-counting when called multiple times across restarts.
	day.RealCost = 0
	day.InputCost = 0
	day.CachedCost = 0
	day.OutputCost = 0
	day.EffectiveTokens = 0
	for model, s := range day.ByModel {
		if s == nil {
			continue
		}
		cached := s.CacheReadTokens
		if s.CachedTokens > cached {
			cached = s.CachedTokens
		}
		if s.EffectiveTokens == 0 {
			s.EffectiveTokens = EffectiveTokens(s.PromptTokens, cached, s.CompletionTokens)
		}
		bd := ComputeCostBreakdown(model, s.PromptTokens, cached, s.CompletionTokens)
		if s.RealCost == 0 {
			s.RealCost = bd.Total
		}
		if s.InputCost == 0 {
			s.InputCost = bd.InputCost
		}
		if s.CachedCost == 0 {
			s.CachedCost = bd.CachedCost
		}
		if s.OutputCost == 0 {
			s.OutputCost = bd.OutputCost
		}
		day.RealCost += s.RealCost
		day.InputCost += s.InputCost
		day.CachedCost += s.CachedCost
		day.OutputCost += s.OutputCost
		day.EffectiveTokens += s.EffectiveTokens
	}
}

func (t *UsageTracker) periodicFlush() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.mu.RLock()
		dirty := t.dirty
		t.mu.RUnlock()
		if dirty {
			t.flushToDisk()
		}
	}
}

func (t *UsageTracker) flushToDisk() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Flush ring buffer
	records := make([]RequestRecord, 0, t.ringCap)
	if t.ringFull {
		for i := t.ringIdx; i < t.ringCap; i++ {
			records = append(records, t.ring[i])
		}
	}
	for i := 0; i < t.ringIdx; i++ {
		records = append(records, t.ring[i])
	}

	data, _ := json.MarshalIndent(records, "", "  ")
	os.WriteFile(t.historyPath, data, 0644)

	dailyData, _ := json.MarshalIndent(t.dailyData, "", "  ")
	os.WriteFile(t.dailyPath, dailyData, 0644)

	t.dirty = false
}

func (t *UsageTracker) pushToRing(r RequestRecord) {
	t.ring[t.ringIdx] = r
	t.ringIdx++
	if t.ringIdx >= t.ringCap {
		t.ringIdx = 0
		t.ringFull = true
	}
}

// Append records a completed request and pushes SSE updates.
func (t *UsageTracker) Append(r RequestRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r.Timestamp = time.Now().UTC().Format(time.RFC3339)
	// Compute real-cost + effective tokens once at ingestion time so every
	// downstream aggregation (daily totals, By* breakdowns, ring buffer) sees
	// the same values without recomputing. Falls back to 0 for unknown models.
	cached := r.CacheReadTokens
	if r.CachedTokens > cached {
		cached = r.CachedTokens
	}
	if r.EffectiveTokens == 0 {
		r.EffectiveTokens = EffectiveTokens(r.InputTokens, cached, r.OutputTokens)
	}
	if r.RealCost == 0 {
		bd := ComputeCostBreakdown(r.Model, r.InputTokens, cached, r.OutputTokens)
		r.RealCost = bd.Total
		r.InputCost = bd.InputCost
		r.CachedCost = bd.CachedCost
		r.OutputCost = bd.OutputCost
	}
	t.pushToRing(r)
	t.dirty = true

	// Update daily aggregation
	dateKey := time.Now().UTC().Format("2006-01-02")
	day, ok := t.dailyData[dateKey]
	if !ok {
		day = &PeriodSummary{}
		t.dailyData[dateKey] = day
	}
	day.Requests++
	day.PromptTokens += r.InputTokens
	day.CompletionTokens += r.OutputTokens
	day.Cost += r.Cost
	day.RealCost += r.RealCost
	day.InputCost += r.InputCost
	day.CachedCost += r.CachedCost
	day.OutputCost += r.OutputCost
	day.EffectiveTokens += r.EffectiveTokens
	// Day-level cache totals must be updated here too, not just in
	// addToSummaryMap — otherwise sumDailyTotalsLocked reads 0 for the
	// headline TotalCacheReadTokens/TotalCachedTokens fields while the
	// ByAccount/ByModel breakdowns (populated via addToSummaryMap) show
	// the real values. This mismatch made the dashboard show "Cached
	// Tokens 0" even when cache hits were being recorded.
	day.CacheReadTokens += r.CacheReadTokens
	day.CacheCreateTokens += r.CacheCreateTokens
	day.CachedTokens += r.CachedTokens
	if r.CacheSource == "upstream" {
		day.UpstreamCacheReadTokens += r.CacheReadTokens
		day.UpstreamCacheCreateTokens += r.CacheCreateTokens
		day.UpstreamCachedTokens += r.CachedTokens
	}
	day.EstimatedCacheReadTokens += r.EstimatedCacheReadTokens
	day.EstimatedCacheCreateTokens += r.EstimatedCacheCreateTokens

	// Per-day breakdowns so the By* tables survive past the ring buffer cap.
	if day.ByModel == nil {
		day.ByModel = make(map[string]*PeriodSummary)
	}
	if day.ByAccount == nil {
		day.ByAccount = make(map[string]*PeriodSummary)
	}
	if day.ByAPIKey == nil {
		day.ByAPIKey = make(map[string]*PeriodSummary)
	}
	if day.ByEndpoint == nil {
		day.ByEndpoint = make(map[string]*PeriodSummary)
	}
	addToSummaryMap(day.ByModel, r.Model, r)
	addToSummaryMap(day.ByAccount, r.AccountID, r)
	if r.APIKeyID != "" {
		addToSummaryMap(day.ByAPIKey, r.APIKeyID, r)
	}
	if r.Endpoint != "" {
		addToSummaryMap(day.ByEndpoint, r.Endpoint, r)
	}
	delete(t.activeReqs, r.AccountID)

	// Push SSE to all listeners (already holding lock, pass data directly)
	activeSnapshot := make([]ActiveRequest, 0, len(t.activeReqs))
	for _, ar := range t.activeReqs {
		activeSnapshot = append(activeSnapshot, ar)
	}
	recentSnapshot := t.getRecentRequestsLocked(time.Now().Add(-5 * time.Minute))
	// A completed request is also a small live event for the Details tab. Keep
	// the existing snapshots in the same payload so Overview clients remain
	// backward-compatible; Details consumes only requestCompleted.
	t.eventSeq++
	eventID := t.eventSeq
	completed := r
	go func(active []ActiveRequest, recent []RequestRecord, request RequestRecord, id uint64) {
		payload := map[string]interface{}{
			"type":             "request.completed",
			"eventId":          id,
			"requestCompleted": request,
			"activeRequests":   active,
			"recentRequests":   recent,
		}
		if data, err := json.Marshal(payload); err == nil {
			broadcastSSEUnsafe(data)
		}
	}(activeSnapshot, recentSnapshot, completed, eventID)
}

// TrackActive marks a request as in-flight. endpoint is the client-facing API
// surface (EndpointClaude/EndpointOpenAI/EndpointOpenAIResponses), NOT the
// upstream account provider — the ActiveRequest.Provider field is only a
// topology display label, so it carries the endpoint here.
func (t *UsageTracker) TrackActive(accountID, endpoint, model string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeReqs[accountID] = ActiveRequest{
		Provider:  endpoint,
		Model:     model,
		AccountID: accountID,
	}
}

// RemoveActive removes an active request (on failure).
func (t *UsageTracker) RemoveActive(accountID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.activeReqs, accountID)
}

// GetStats compiles usage statistics for a given period.
func (t *UsageTracker) GetStats(period string) *UsageStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	cutoff := getPeriodCutoff(period)

	stats := &UsageStats{
		ByModel:    make(map[string]*PeriodSummary),
		ByAccount:  make(map[string]*PeriodSummary),
		ByAPIKey:   make(map[string]*PeriodSummary),
		ByEndpoint: make(map[string]*PeriodSummary),
	}

	// Recent requests stay sourced from the ring buffer — it is the live
	// "recent activity" feed, intentionally capped at ringCap.
	stats.RecentRequests = t.getRecentRequestsLocked(cutoff)

	// Headline totals AND the By* breakdowns come from the uncapped daily
	// aggregation, summed over the days inside the requested period. This is what
	// "đếm dồn theo thời gian" should show and keeps the tables growing past the
	// ring buffer's ringCap cap (the "stuck at 500" bug). Days persisted before
	// per-day breakdowns existed contribute to the totals but carry no By* detail.
	t.sumDailyTotalsLocked(stats, period)

	// Active requests
	stats.ActiveRequests = make([]ActiveRequest, 0, len(t.activeReqs))
	for _, ar := range t.activeReqs {
		stats.ActiveRequests = append(stats.ActiveRequests, ar)
	}

	// Build account name map from recent requests + config accounts
	stats.AccountNames = make(map[string]string)
	for _, rec := range stats.RecentRequests {
		if rec.AccountName != "" && rec.AccountID != "" {
			if _, exists := stats.AccountNames[rec.AccountID]; !exists {
				stats.AccountNames[rec.AccountID] = rec.AccountName
			}
		}
	}
	// Also populate names from config for accounts that have no recent requests
	for _, a := range config.GetAccounts() {
		if _, exists := stats.AccountNames[a.ID]; !exists {
			name := ""
			if a.Nickname != "" {
				name = a.Nickname
			} else if a.Email != "" {
				name = a.Email
			} else if len(a.ID) >= 8 {
				name = a.ID[:8]
			} else {
				name = a.ID
			}
			stats.AccountNames[a.ID] = name
		}
	}

	return stats
}

// TokensForAccountSince returns the persisted input plus output token total for
// one account from the UTC day containing since through the current day. Daily
// usage is the durable accounting source: the in-memory request ring is capped
// at 500 records and must not be used for a quota window that can span days.
func (t *UsageTracker) TokensForAccountSince(accountID string, since time.Time) int {
	if t == nil || accountID == "" || since.IsZero() {
		return 0
	}

	cutoffDate := since.UTC().Format("2006-01-02")
	t.mu.RLock()
	defer t.mu.RUnlock()

	tokens := 0
	for dateKey, day := range t.dailyData {
		if dateKey < cutoffDate || day == nil || day.ByAccount == nil {
			continue
		}
		if account, ok := day.ByAccount[accountID]; ok && account != nil {
			tokens += account.PromptTokens + account.CompletionTokens
		}
	}
	return tokens
}

// GetChartData produces time-bucketed chart data.
func (t *UsageTracker) GetChartData(period string) []ChartDataPoint {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()
	switch period {
	case "today":
		return t.bucketByHour(now, true)
	case "24h":
		return t.bucketByHour(now, false)
	case "7d":
		return t.bucketByDay(now, 7)
	case "30d":
		return t.bucketByDay(now, 30)
	default:
		return t.bucketByDay(now, 7)
	}
}

func (t *UsageTracker) bucketByHour(now time.Time, today bool) []ChartDataPoint {
	buckets := 24
	if today {
		now = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	startTime := now.Add(-time.Duration(buckets) * time.Hour)
	points := make([]ChartDataPoint, buckets)
	for i := 0; i < buckets; i++ {
		ts := startTime.Add(time.Duration(i) * time.Hour)
		points[i].Label = ts.Format("15:04")
	}

	records := t.getAllRecordsLocked()
	for _, rec := range records {
		recTime, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil {
			continue
		}
		if recTime.Before(startTime) || recTime.After(now) {
			continue
		}
		idx := int(recTime.Sub(startTime).Hours())
		if idx >= 0 && idx < buckets {
			points[idx].Tokens += rec.InputTokens + rec.OutputTokens
			points[idx].Cost += rec.Cost
		}
	}
	return points
}

func (t *UsageTracker) bucketByDay(now time.Time, days int) []ChartDataPoint {
	points := make([]ChartDataPoint, days)
	for i := 0; i < days; i++ {
		d := now.Add(-time.Duration(days-1-i) * 24 * time.Hour)
		dateKey := d.Format("2006-01-02")
		points[i].Label = d.Format("Jan 2")
		if day, ok := t.dailyData[dateKey]; ok {
			points[i].Tokens = day.PromptTokens + day.CompletionTokens
			points[i].Cost = day.Cost
		}
	}
	return points
}

func (t *UsageTracker) getRecentRequestsLocked(cutoff time.Time) []RequestRecord {
	var result []RequestRecord
	if t.ringFull {
		for i := t.ringIdx; i < t.ringCap; i++ {
			if r := t.ring[i]; r.Timestamp != "" {
				if rt, err := time.Parse(time.RFC3339, r.Timestamp); err == nil && rt.After(cutoff) {
					result = append(result, r)
				}
			}
		}
	}
	for i := 0; i < t.ringIdx; i++ {
		if r := t.ring[i]; r.Timestamp != "" {
			if rt, err := time.Parse(time.RFC3339, r.Timestamp); err == nil && rt.After(cutoff) {
				result = append(result, r)
			}
		}
	}
	// Reverse to show newest first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (t *UsageTracker) getAllRecordsLocked() []RequestRecord {
	var result []RequestRecord
	if t.ringFull {
		result = append(result, t.ring[t.ringIdx:]...)
	}
	result = append(result, t.ring[:t.ringIdx]...)
	return result
}

// addToSummaryMap accumulates a single request into a breakdown map keyed by
// model/account/apikey/endpoint. Used to build the per-day breakdowns stored on
// each daily bucket so the By* tables are not capped by the ring buffer.
func addToSummaryMap(m map[string]*PeriodSummary, key string, r RequestRecord) {
	if key == "" {
		return
	}
	s, ok := m[key]
	if !ok {
		s = &PeriodSummary{}
		m[key] = s
	}
	s.Requests++
	s.PromptTokens += r.InputTokens
	s.CompletionTokens += r.OutputTokens
	s.Cost += r.Cost
	s.RealCost += r.RealCost
	s.InputCost += r.InputCost
	s.CachedCost += r.CachedCost
	s.OutputCost += r.OutputCost
	s.EffectiveTokens += r.EffectiveTokens
	s.CacheReadTokens += r.CacheReadTokens
	s.CacheCreateTokens += r.CacheCreateTokens
	s.CachedTokens += r.CachedTokens
	if r.CacheSource == "upstream" {
		s.UpstreamCacheReadTokens += r.CacheReadTokens
		s.UpstreamCacheCreateTokens += r.CacheCreateTokens
		s.UpstreamCachedTokens += r.CachedTokens
	}
	s.EstimatedCacheReadTokens += r.EstimatedCacheReadTokens
	s.EstimatedCacheCreateTokens += r.EstimatedCacheCreateTokens
}

// mergeSummaryInto folds one source breakdown map into a destination, summing
// the per-key totals. Used to combine multiple days' breakdowns over a period.
func mergeSummaryMapInto(dst, src map[string]*PeriodSummary) {
	for key, s := range src {
		if s == nil {
			continue
		}
		d, ok := dst[key]
		if !ok {
			d = &PeriodSummary{}
			dst[key] = d
		}
		d.Requests += s.Requests
		d.PromptTokens += s.PromptTokens
		d.CompletionTokens += s.CompletionTokens
		d.Cost += s.Cost
		d.RealCost += s.RealCost
		d.InputCost += s.InputCost
		d.CachedCost += s.CachedCost
		d.OutputCost += s.OutputCost
		d.EffectiveTokens += s.EffectiveTokens
		d.CacheReadTokens += s.CacheReadTokens
		d.CacheCreateTokens += s.CacheCreateTokens
		d.CachedTokens += s.CachedTokens
		d.UpstreamCacheReadTokens += s.UpstreamCacheReadTokens
		d.UpstreamCacheCreateTokens += s.UpstreamCacheCreateTokens
		d.UpstreamCachedTokens += s.UpstreamCachedTokens
		d.EstimatedCacheReadTokens += s.EstimatedCacheReadTokens
		d.EstimatedCacheCreateTokens += s.EstimatedCacheCreateTokens
	}
}

// sumDailyTotalsLocked fills the headline totals (TotalRequests/tokens/cost)
// from the uncapped daily aggregation, summing only the days that fall within
// the requested period. dailyData is keyed by UTC date ("2006-01-02"). The ring
// buffer is capped at ringCap and must not be used for these lifetime-style
// totals (that was the "stuck at 500" bug). Caller must hold t.mu.
func (t *UsageTracker) sumDailyTotalsLocked(stats *UsageStats, period string) {
	cutoffDate := dailyCutoffDate(period)
	for dateKey, day := range t.dailyData {
		if day == nil {
			continue
		}
		if cutoffDate != "" && dateKey < cutoffDate {
			continue
		}
		stats.TotalRequests += day.Requests
		stats.TotalPromptTokens += day.PromptTokens
		stats.TotalCompletionTokens += day.CompletionTokens
		stats.TotalCost += day.Cost
		stats.TotalRealCost += day.RealCost
		stats.TotalInputCost += day.InputCost
		stats.TotalCachedCost += day.CachedCost
		stats.TotalOutputCost += day.OutputCost
		stats.TotalEffectiveTokens += day.EffectiveTokens
		stats.TotalCacheReadTokens += day.CacheReadTokens
		stats.TotalCacheCreateTokens += day.CacheCreateTokens
		stats.TotalCachedTokens += day.CachedTokens
		stats.TotalUpstreamCacheReadTokens += day.UpstreamCacheReadTokens
		stats.TotalUpstreamCacheCreateTokens += day.UpstreamCacheCreateTokens
		stats.TotalUpstreamCachedTokens += day.UpstreamCachedTokens
		stats.TotalEstimatedCacheReadTokens += day.EstimatedCacheReadTokens
		stats.TotalEstimatedCacheCreateTokens += day.EstimatedCacheCreateTokens

		// Merge each day's per-dimension breakdown so the By* tables are also
		// lifetime-accurate instead of capped at the ring buffer size. Legacy
		// daily buckets written before this field existed have nil maps and
		// simply contribute nothing to the breakdown (their totals still count).
		mergeSummaryMapInto(stats.ByModel, day.ByModel)
		mergeSummaryMapInto(stats.ByAccount, day.ByAccount)
		mergeSummaryMapInto(stats.ByAPIKey, day.ByAPIKey)
		mergeSummaryMapInto(stats.ByEndpoint, day.ByEndpoint)
	}
}

// dailyCutoffDate returns the earliest UTC date ("2006-01-02") to include for a
// period, or "" to include every day. Daily buckets have day granularity, so
// "today" only counts the current UTC day, while "24h" rolls back one UTC day
// so it also includes (part of) yesterday — matching the rolling-24h semantics
// used by getPeriodCutoff for the recent-requests feed and chart.
func dailyCutoffDate(period string) string {
	now := time.Now().UTC()
	switch period {
	case "all":
		return ""
	case "today":
		return now.Format("2006-01-02")
	case "24h":
		return now.AddDate(0, 0, -1).Format("2006-01-02")
	case "7d":
		return now.AddDate(0, 0, -6).Format("2006-01-02")
	case "30d":
		return now.AddDate(0, 0, -29).Format("2006-01-02")
	case "60d":
		return now.AddDate(0, 0, -59).Format("2006-01-02")
	default:
		return now.AddDate(0, 0, -6).Format("2006-01-02")
	}
}

// SSE broadcasting
type sseListener struct {
	ch chan []byte
}

var (
	sseListeners   []*sseListener
	sseListenersMu sync.RWMutex
)

func broadcastSSEUnsafe(data []byte) {
	sseListenersMu.RLock()
	for _, l := range sseListeners {
		select {
		case l.ch <- data:
		default:
		}
	}
	sseListenersMu.RUnlock()
}

func (t *UsageTracker) broadcastStats() {
	stats := t.buildQuickStats()
	data, err := json.Marshal(stats)
	if err != nil {
		return
	}
	broadcastSSEUnsafe(data)
}

func (t *UsageTracker) buildQuickStats() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	activeReqs := make([]ActiveRequest, 0, len(t.activeReqs))
	for _, ar := range t.activeReqs {
		activeReqs = append(activeReqs, ar)
	}

	recent := t.getRecentRequestsLocked(time.Now().Add(-5 * time.Minute))

	return map[string]interface{}{
		"activeRequests": activeReqs,
		"recentRequests": recent,
	}
}

func (t *UsageTracker) SubscribeSSE() *sseListener {
	l := &sseListener{ch: make(chan []byte, 16)}
	sseListenersMu.Lock()
	sseListeners = append(sseListeners, l)
	sseListenersMu.Unlock()
	return l
}

func (t *UsageTracker) UnsubscribeSSE(l *sseListener) {
	sseListenersMu.Lock()
	defer sseListenersMu.Unlock()
	for i, ls := range sseListeners {
		if ls == l {
			sseListeners = append(sseListeners[:i], sseListeners[i+1:]...)
			break
		}
	}
}

func getPeriodCutoff(period string) time.Time {
	now := time.Now()
	switch period {
	case "all":
		return time.Time{} // zero time includes all records
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "30d":
		return now.Add(-30 * 24 * time.Hour)
	case "60d":
		return now.Add(-60 * 24 * time.Hour)
	default:
		return now.Add(-24 * time.Hour)
	}
}

// Ensure tracker reference is accessible from handler
var trackerInstance *UsageTracker

func InitUsageTracker() {
	trackerInstance = GetUsageTracker()
}

func GetTracker() *UsageTracker {
	return trackerInstance
}

// SetTrackerErrorProvider sets the error provider name for the topology.
func (t *UsageTracker) SetErrorProvider(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Just store for SSE broadcast; handled in buildQuickStats via recent requests
	logger.Debugf("[Usage] Error provider: %s", provider)
}
