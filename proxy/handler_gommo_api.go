// Package proxy: handler_gommo_api.go
//
// Admin endpoints for Gommo AutoAI media accounts: import, balance refresh,
// catalog refresh, and the video-generation route.
//
// Video is exposed on its own path rather than an OpenAI-compatible one because
// OpenAI has no video-generation endpoint to mirror. The route returns the job
// id alongside the URL so a caller whose render outlives the request deadline
// can still retrieve the result.
package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"time"

	"github.com/google/uuid"
)

// apiImportGommoProvider registers a Gommo account. Both the access token and
// the domain are required: the API rejects a call that omits either, so an
// account saved without a domain could never serve a request.
func (h *Handler) apiImportGommoProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string   `json:"accessToken"`
		Domain       string   `json:"domain"`
		BaseURL      string   `json:"baseUrl"`
		Nickname     string   `json:"nickname"`
		ProjectID    string   `json:"projectId"`
		Capabilities []string `json:"capabilities"`
		TTSModel     string   `json:"ttsModel"`
		VoiceID      string   `json:"voiceId"`
		ImageModel   string   `json:"imageModel"`
		Weight       int      `json:"weight"`
		ProxyURL     string   `json:"proxyURL"`
		Test         bool     `json:"test"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	token := strings.TrimSpace(req.AccessToken)
	domain := strings.TrimSpace(req.Domain)
	if token == "" || domain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "accessToken and domain are required"})
		return
	}

	capabilities := normalizeGommoCapabilities(req.Capabilities)
	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = gommoProviderLabel + " (" + domain + ")"
	}

	account := config.Account{
		ID:          uuid.New().String(),
		Email:       domain,
		Nickname:    nickname,
		AuthMethod:  gommoAuthMethod,
		Provider:    gommoProviderLabel,
		AccessToken: token,
		BaseURL:     strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"),
		Region:      "external",
		Enabled:     true,
		MachineId:   config.GenerateMachineId(),
		Weight:      req.Weight,
		ProxyURL:    strings.TrimSpace(req.ProxyURL),
		// Capabilities partitions the pool. Gommo generates media and never
		// serves chat, so it must not carry the "chat" capability: the chat pool
		// would otherwise route a completion to an API that cannot answer one.
		Capabilities:   capabilities,
		ProviderKind:   capabilities[0],
		GommoDomain:    domain,
		GommoProjectID: strings.TrimSpace(req.ProjectID),
		GommoTTSModel:  strings.TrimSpace(req.TTSModel),
		GommoVoiceID:   strings.TrimSpace(req.VoiceID),
		ImageModel:     strings.TrimSpace(req.ImageModel),
		// No ExpiresAt: the Gommo credential is a long-lived token with no
		// refresh grant, so tokenNeedsRefresh must treat it as always valid.
	}

	var testErr string
	if req.Test {
		if err := refreshGommoAccount(&account); err != nil {
			testErr = err.Error()
		}
	}

	if err := config.AddAccount(account); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()

	// The catalog call is three round-trips; run it after responding so the UI
	// is not held open by a slow upstream.
	acc := account
	safeGo("fetchAndCacheAccountModels/gommo", func() {
		if err := h.fetchAndCacheAccountModels(&acc); err != nil {
			logger.Warnf("[Gommo] model catalog fetch failed for %s: %v", acc.Email, err)
		}
	})

	response := map[string]interface{}{
		"success": true,
		"account": gommoAccountJSON(&account),
	}
	if req.Test {
		response["test"] = map[string]interface{}{"error": testErr}
	}
	writeJSON(w, http.StatusOK, response)
}

// normalizeGommoCapabilities keeps only the media capabilities this provider can
// actually serve, and never returns an empty set: an account with no capability
// is invisible to every pool and would silently never be selected.
func normalizeGommoCapabilities(requested []string) []string {
	allowed := map[string]bool{
		capabilityImage:      true,
		capabilityVideo:      true,
		capabilityAudioTTS:   true,
		capabilityAudioMusic: true,
	}
	out := make([]string, 0, len(allowed))
	seen := make(map[string]bool)
	for _, value := range requested {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "tts" || normalized == "audio" {
			normalized = capabilityAudioTTS
		}
		if allowed[normalized] && !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{capabilityImage, capabilityVideo, capabilityAudioTTS, capabilityAudioMusic}
	}
	return out
}

func gommoAccountJSON(account *config.Account) map[string]interface{} {
	return map[string]interface{}{
		"id":           account.ID,
		"email":        account.Email,
		"nickname":     account.Nickname,
		"provider":     account.Provider,
		"domain":       account.GommoDomain,
		"projectId":    account.GommoProjectID,
		"capabilities": account.Capabilities,
		"creditsAi":    account.GommoCreditsAI,
		"balance":      account.GommoBalance,
		"currency":     account.GommoCurrency,
		"checkedAt":    account.GommoCreditsCheckedAt,
	}
}

// apiRefreshGommoBalance re-reads the account endpoint on demand.
func (h *Handler) apiRefreshGommoBalance(w http.ResponseWriter, r *http.Request, id string) {
	account := h.pool.GetByID(id)
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Account not found"})
		return
	}
	if !isGommoAccount(account) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a Gommo account"})
		return
	}
	if err := refreshGommoAccount(account); err != nil {
		writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	if latest := h.pool.GetByID(id); latest != nil {
		account = latest
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"account": gommoAccountJSON(account),
	})
}

// ==================== Video generation ====================

// apiGommoCreateVideo serves POST /v1/videos/generations.
//
// The path is OmniProxy's own: there is no OpenAI video endpoint to mirror, so
// the request and response shapes follow the Images API conventions (prompt,
// model, data[]) to stay predictable for clients already speaking that dialect.
func (h *Handler) apiGommoCreateVideo(w http.ResponseWriter, r *http.Request) {
	var in gommoVideoRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string

	for {
		account := h.pool.GetNextForCapability(capabilityVideo, "", excluded)
		if account == nil {
			break
		}
		excluded[account.ID] = true
		if !isGommoAccount(account) {
			continue
		}
		lastAccountID = account.ID

		job, err := callGommoVideo(r, account, in)
		if err != nil {
			lastErr = err
			h.pool.RecordError(account.ID, serviceErrorIsQuota(err), capabilityVideo)
			logger.Infof("[Gommo] video via %s failed: %v", account.Email, err)
			continue
		}

		h.pool.RecordSuccess(account.ID, capabilityVideo)
		h.recordUsage(apiKeyID, account.ID, in.Model, endpointVideo, 0, 0, 0, 0, 0, 0)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"created": time.Now().Unix(),
			"id":      job.ID,
			"status":  job.Status,
			"data":    []map[string]string{{"url": job.URL}},
		})
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error",
			"No available accounts with video capability. Add a Gommo account with the video capability enabled.")
		return
	}
	h.recordError(apiKeyID, lastAccountID, in.Model, endpointVideo, lastErr.Error())
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

// apiGommoVideoStatus serves GET /v1/videos/{id}, so a caller whose render
// outlived the create request can collect the result instead of losing it.
func (h *Handler) apiGommoVideoStatus(w http.ResponseWriter, r *http.Request, jobID string) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "video id is required")
		return
	}

	excluded := make(map[string]bool)
	var lastErr error
	for {
		account := h.pool.GetNextForCapability(capabilityVideo, "", excluded)
		if account == nil {
			break
		}
		excluded[account.ID] = true
		if !isGommoAccount(account) {
			continue
		}
		job, err := gommoVideoStatus(r, account, jobID)
		if err != nil {
			lastErr = err
			continue
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":     job.ID,
			"status": job.Status,
			"data":   []map[string]string{{"url": job.URL}},
		})
		return
	}
	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts with video capability")
		return
	}
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

// ==================== Music generation ====================

func (h *Handler) apiGommoCreateMusic(w http.ResponseWriter, r *http.Request) {
	var in gommoMusicRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
	for {
		account := h.pool.GetNextForCapability(capabilityAudioMusic, "", excluded)
		if account == nil {
			break
		}
		excluded[account.ID] = true
		if !isGommoAccount(account) {
			continue
		}
		lastAccountID = account.ID
		job, err := callGommoMusic(r, account, in)
		if err != nil {
			lastErr = err
			h.pool.RecordError(account.ID, serviceErrorIsQuota(err), capabilityAudioMusic)
			logger.Infof("[Gommo] music via %s failed: %v", account.Email, err)
			continue
		}
		h.pool.RecordSuccess(account.ID, capabilityAudioMusic)
		h.recordUsage(apiKeyID, account.ID, in.Model, endpointMusic, 0, 0, 0, 0, 0, 0)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"created": time.Now().Unix(), "id": job.ID, "status": job.Status,
			"data": []map[string]string{{"url": job.URL}},
		})
		return
	}
	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts with audio-music capability")
		return
	}
	h.recordError(apiKeyID, lastAccountID, in.Model, endpointMusic, lastErr.Error())
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

func (h *Handler) apiGommoMusicStatus(w http.ResponseWriter, r *http.Request, jobID string) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "music id is required")
		return
	}
	excluded := make(map[string]bool)
	var lastErr error
	for {
		account := h.pool.GetNextForCapability(capabilityAudioMusic, "", excluded)
		if account == nil {
			break
		}
		excluded[account.ID] = true
		if !isGommoAccount(account) {
			continue
		}
		job, err := gommoMusicStatus(r, account, jobID)
		if err != nil {
			lastErr = err
			continue
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id": job.ID, "status": job.Status, "data": []map[string]string{{"url": job.URL}},
		})
		return
	}
	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts with audio-music capability")
		return
	}
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

// ==================== Admin media playground ====================

// The playground targets one named account instead of the pool: an operator
// testing the credential they just added needs that account exercised, not
// whichever one happens to be next in rotation.
type gommoPlaygroundRequest struct {
	AccountID  string `json:"accountId"`
	Kind       string `json:"kind"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model"`
	Size       string `json:"size"`
	Ratio      string `json:"ratio"`
	Resolution string `json:"resolution"`
	Duration   string `json:"duration"`
	Mode       string `json:"mode"`
	Voice      string `json:"voice"`
	JobID      string `json:"jobId"`
	// Music takes a song name and optional lyrics alongside the styles text the
	// shared Prompt field carries.
	Title  string `json:"title"`
	Lyrics string `json:"lyrics"`
	N      int    `json:"n"`
	// Images seeds image-to-video: several video models reject a text-only
	// prompt and demand a first frame, so the playground has to be able to
	// supply one.
	Images []string `json:"images"`
}

func (h *Handler) gommoPlaygroundAccount(id string) (*config.Account, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("accountId is required")
	}
	account := h.pool.GetByID(id)
	if account == nil {
		return nil, fmt.Errorf("account not found")
	}
	if !isGommoAccount(account) {
		return nil, fmt.Errorf("account %s is not a Gommo account", account.Email)
	}
	return account, nil
}

// apiGommoModels groups the provider's catalog by media type so the playground
// can offer only the models each field can actually accept.
func (h *Handler) apiGommoModels(w http.ResponseWriter, r *http.Request, id string) {
	account, err := h.gommoPlaygroundAccount(id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	models, err := fetchGommoModels(account)
	if err != nil {
		writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	grouped := map[string][]map[string]string{}
	for _, model := range models {
		kind := "image"
		if len(model.OutputTypes) > 0 {
			kind = model.OutputTypes[0]
		}
		grouped[kind] = append(grouped[kind], map[string]string{"id": model.ModelId, "name": model.ModelName})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"models":   grouped,
		"voiceId":  account.GommoVoiceID,
		"ttsModel": account.GommoTTSModel,
	})
}

// writeGommoJobStatus answers a status lookup. An unknown job id comes back from
// upstream as an empty envelope, which must not be reported as a finished render
// with a blank URL, so a job with neither status nor URL is a 404 instead.
func writeGommoJobStatus(w http.ResponseWriter, kind string, job gommoJob) {
	if job.Status == "" && job.URL == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "gommo " + kind + " status: job not found"})
		return
	}
	urls := []string{}
	if job.URL != "" {
		urls = append(urls, job.URL)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "kind": kind, "id": job.ID, "status": job.Status, "urls": urls,
	})
}

// apiGommoPlaygroundRun runs one generation against one account and returns the
// artifact as a URL (image, video) or an inline data URL (speech), so the admin
// page can render the result without a second credentialed fetch.
func (h *Handler) apiGommoPlaygroundRun(w http.ResponseWriter, r *http.Request) {
	var in gommoPlaygroundRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	account, err := h.gommoPlaygroundAccount(in.AccountID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	started := time.Now()

	switch strings.ToLower(strings.TrimSpace(in.Kind)) {
	case "image":
		result, err := callGommoImage(r, account, imageGenerationRequest{
			Prompt: in.Prompt, Model: in.Model, Size: in.Size, N: in.N,
		})
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		urls := make([]string, 0, len(result.Data))
		for _, item := range result.Data {
			urls = append(urls, item.URL)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true, "kind": "image", "urls": urls,
			"elapsedMs": time.Since(started).Milliseconds(),
		})

	case "video":
		// A render that outlives the poll ceiling still returns its job id, so
		// the caller can collect it later instead of paying for it twice.
		job, err := callGommoVideo(r, account, gommoVideoRequest{
			Prompt: in.Prompt, Model: in.Model, Ratio: in.Ratio,
			Resolution: in.Resolution, Duration: in.Duration,
			Mode: in.Mode, Images: in.Images,
		})
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true, "kind": "video", "id": job.ID, "status": job.Status,
			"urls": []string{job.URL}, "elapsedMs": time.Since(started).Milliseconds(),
		})

	case "video-status":
		job, err := gommoVideoStatus(r, account, in.JobID)
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeGommoJobStatus(w, "video", job)

	case "music":
		job, err := callGommoMusic(r, account, gommoMusicRequest{
			Prompt: in.Prompt, Model: in.Model,
			Title: in.Title, Lyrics: in.Lyrics,
		})
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true, "kind": "music", "id": job.ID, "status": job.Status,
			"urls": []string{job.URL}, "elapsedMs": time.Since(started).Milliseconds(),
		})

	case "music-status":
		job, err := gommoMusicStatus(r, account, in.JobID)
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeGommoJobStatus(w, "music", job)

	case "tts":
		audio, mime, err := callGommoTTS(r, account, gommoTTSRequest{
			Input: in.Prompt, Model: in.Model, Voice: in.Voice,
		})
		if err != nil {
			writeJSON(w, serviceErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		if mime == "" {
			mime = "audio/mpeg"
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true, "kind": "audio",
			"dataUrl":   "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(audio),
			"bytes":     len(audio),
			"elapsedMs": time.Since(started).Milliseconds(),
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be image, video, video-status, music, music-status or tts"})
	}
}
