// Package proxy: external_gommo.go
//
// Gommo AutoAI media provider (video / image / text-to-speech / music). The
// 79ai.net front end is a different UI over this same api.gommo.net backend, so
// an account from either place uses this adapter.
//
// Gommo is not OpenAI-compatible. Every call is a form-urlencoded POST whose
// body carries `domain` alongside the request parameters, and the generative
// endpoints are asynchronous: create returns a job id (`id_base`) whose status
// is polled on a second endpoint until a URL appears. This file adapts that
// shape to the OpenAI-style routes OmniProxy already serves, so downstream
// clients need no Gommo-specific code.
//
// The gateway accepts the credential from four places and states its own
// preference: "Prefer Authorization: Bearer <token>. Other token sources are
// fallbacks only." Both the header and the `access_token` body field are
// therefore sent — the header because it is the documented path, the field
// because older deployments read only the body.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"time"
)

const gommoAuthMethod = "gommo"

// gommoProviderLabel is the display/provider name shown for Gommo accounts and
// stamped onto the model catalog entries they contribute.
const gommoProviderLabel = "Gommo AutoAI"

// gommoDefaultBaseURL is the published API root. A per-account BaseURL override
// exists for operators pointed at a different deployment.
const gommoDefaultBaseURL = "https://api.gommo.net"

// gommoPollInterval and gommoPollTimeout bound how long a synchronous
// OmniProxy request waits on an asynchronous Gommo job. Video renders are the
// slow case; a request that outlives the ceiling returns the job id so the
// caller can poll it themselves instead of holding the connection open.
//
// The interval is a var rather than a const only so tests can shorten it; no
// production path reassigns it.
var (
	gommoPollInterval = 3 * time.Second
	gommoPollTimeout  = 10 * time.Minute
)

// gommoPollIntervalForTest shortens the poll interval and returns a function
// that restores the previous value.
func gommoPollIntervalForTest(d time.Duration) func() {
	previous := gommoPollInterval
	gommoPollInterval = d
	return func() { gommoPollInterval = previous }
}

// gommoPollTimeoutForTest shortens the poll ceiling and returns a function that
// restores the previous value. The timeout branch is the one an operator hits on
// a slow render, so it needs a test rather than only a code path.
func gommoPollTimeoutForTest(d time.Duration) func() {
	previous := gommoPollTimeout
	gommoPollTimeout = d
	return func() { gommoPollTimeout = previous }
}

const (
	// maxGommoResponseBytes bounds a JSON control response. Media itself is
	// fetched from the returned URL, not inlined, so this stays small.
	maxGommoResponseBytes = 8 << 20
	// maxGommoMediaBytes bounds a media download performed on the caller's
	// behalf (TTS audio returned as raw bytes).
	maxGommoMediaBytes = 64 << 20
)

// isGommoAccount reports whether the account routes to the Gommo media API.
func isGommoAccount(account *config.Account) bool {
	return account != nil && account.AuthMethod == gommoAuthMethod
}

// gommoBaseURL resolves the API root for an account.
func gommoBaseURL(account *config.Account) string {
	if account != nil {
		if base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/"); base != "" {
			return base
		}
	}
	return gommoDefaultBaseURL
}

// Gommo endpoint paths, grouped by the capability they serve.
const (
	gommoPathModels      = "/ai/models"
	gommoPathCreateVideo = "/ai/create-video"
	gommoPathVideo       = "/ai/video"
	gommoPathCreateImage = "/ai/generateImage"
	gommoPathImage       = "/ai/image"
	gommoPathImageUpload = "/ai/image-upload"
	gommoPathAudio       = "/ai/audio"
	gommoPathMe          = "/api/apps/go-mmo/ai/me"
	gommoPathTools       = "/api/apps/go-mmo/ai_templates/tools"
	gommoPathCreateMusic = "/api/apps/go-mmo/ai_musics/create"
	gommoPathMusicInfo   = "/api/apps/go-mmo/ai_musics/getInfo"
)

// gommoForm builds the shared body every Gommo call requires. The credential
// travels in the form body rather than a header, which is the provider's own
// contract; `domain` identifies the calling deployment and is rejected if absent.
func gommoForm(account *config.Account, params map[string]interface{}) (url.Values, error) {
	token := strings.TrimSpace(account.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("gommo account %s has no access token", account.Email)
	}
	domain := strings.TrimSpace(account.GommoDomain)
	if domain == "" {
		return nil, fmt.Errorf("gommo account %s has no domain configured", account.Email)
	}
	form := url.Values{}
	form.Set("access_token", token)
	form.Set("domain", domain)
	for key, value := range params {
		encoded, ok := gommoFormValue(value)
		if !ok {
			continue
		}
		form.Set(key, encoded)
	}
	return form, nil
}

// gommoFormValue encodes one parameter. Nested values are JSON-encoded because
// the API expects composite fields (images[], subjects[], voice_settings) as
// JSON strings inside a form body. Empty values are dropped rather than sent
// blank: the upstream validates several fields as "present implies meaningful".
func gommoFormValue(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case nil:
		return "", false
	case string:
		trimmed := strings.TrimSpace(typed)
		return trimmed, trimmed != ""
	case bool:
		if !typed {
			return "false", true
		}
		return "true", true
	case int:
		if typed == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", typed), true
	case int64:
		if typed == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", typed), true
	case float64:
		if typed == 0 {
			return "", false
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", typed), "0"), "."), true
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", false
		}
		text := string(encoded)
		// A marshalled empty container carries no instruction to the upstream.
		if text == "null" || text == "[]" || text == "{}" {
			return "", false
		}
		return text, true
	}
}

// gommoPost performs one form-urlencoded call and returns the raw JSON body.
//
// Gommo reports failures in two different shapes: an HTTP error status, and an
// HTTP 200 whose body carries an `error` field. Both are surfaced as
// serviceHTTPError so the existing capability failover treats them alike.
func gommoPost(parent *http.Request, account *config.Account, path string, params map[string]interface{}) ([]byte, error) {
	form, err := gommoForm(account, params)
	if err != nil {
		return nil, err
	}
	endpoint := gommoBaseURL(account) + path

	var req *http.Request
	body := strings.NewReader(form.Encode())
	if parent != nil {
		req, err = http.NewRequestWithContext(parent.Context(), http.MethodPost, endpoint, body)
	} else {
		req, err = http.NewRequest(http.MethodPost, endpoint, body)
	}
	if err != nil {
		return nil, fmt.Errorf("gommo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The gateway accepts the token from four places and documents its own
	// order: "Prefer Authorization: Bearer <token>. Other token sources are
	// fallbacks only." Sending the header as well as the body field follows that
	// preference while keeping the form field working for the deployments that
	// only read the body.
	if token := strings.TrimSpace(account.AccessToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		_ = config.UpdateAccountServiceStats(account.ID, 0, true, false, nil)
		return nil, fmt.Errorf("gommo %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxGommoResponseBytes))
	headers := serviceResponseHeaders(resp.Header)
	if readErr != nil {
		_ = config.UpdateAccountServiceStats(account.ID, resp.StatusCode, true, false, headers)
		return nil, fmt.Errorf("gommo %s read: %w", path, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		isQuota := resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests
		_ = config.UpdateAccountServiceStats(account.ID, resp.StatusCode, true, isQuota, headers)
		return nil, &serviceHTTPError{Status: resp.StatusCode, Body: string(raw), Headers: headers}
	}
	if message := gommoErrorMessage(raw); message != "" {
		// A 200 carrying an error is still a failure; classify insufficient
		// credit as quota so the pool rotates instead of retrying the same key.
		status := http.StatusBadGateway
		if gommoMessageIsQuota(message) {
			status = http.StatusPaymentRequired
		}
		_ = config.UpdateAccountServiceStats(account.ID, status, true, status == http.StatusPaymentRequired, headers)
		return nil, &serviceHTTPError{Status: status, Body: message, Headers: headers}
	}
	_ = config.UpdateAccountServiceStats(account.ID, resp.StatusCode, false, false, headers)
	return raw, nil
}

// gommoErrorMessage reports the error carried by an HTTP 200 body, or "".
// The field is typed inconsistently upstream (a numeric code on some endpoints,
// a string on others), so both spellings are accepted.
func gommoErrorMessage(raw []byte) string {
	var envelope struct {
		Error   interface{} `json:"error"`
		Message string      `json:"message"`
		Success *bool       `json:"success"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	hasError := false
	switch typed := envelope.Error.(type) {
	case string:
		hasError = strings.TrimSpace(typed) != ""
	case float64:
		hasError = typed != 0
	case bool:
		hasError = typed
	}
	if !hasError && envelope.Success != nil && !*envelope.Success {
		hasError = true
	}
	if !hasError {
		return ""
	}
	if message := strings.TrimSpace(envelope.Message); message != "" {
		return message
	}
	if text, ok := envelope.Error.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return "gommo request failed"
}

// gommoMessageIsQuota recognises the upstream's out-of-credit vocabulary so a
// billing failure rotates accounts rather than being retried as transient.
func gommoMessageIsQuota(message string) bool {
	lower := strings.ToLower(message)
	for _, phrase := range []string{
		"credit", "balance", "insufficient", "not enough",
		"hết credit", "không đủ", "quota", "limit reached",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// ==================== Asynchronous job polling ====================

// gommoJob is the normalized view of a create/status response. Gommo nests the
// same fields under different keys per media type (videoInfo, imageInfo,
// musicInfo, audioInfo) and sometimes returns them at the top level, so each
// caller extracts its own shape and hands the result here.
type gommoJob struct {
	ID     string
	Status string
	URL    string
	Prompt string
}

// gommoTerminalStatus reports whether a job status means "stop polling", and
// whether it succeeded. An unknown status is treated as still-running: guessing
// "failed" on a status the provider added later would abort a valid render.
func gommoTerminalStatus(status string) (done bool, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCESS", "SUCCEEDED", "COMPLETED", "COMPLETE", "DONE", "FINISHED":
		return true, true
	case "FAILED", "FAIL", "ERROR", "CANCELLED", "CANCELED", "REJECTED", "TIMEOUT":
		return true, false
	default:
		return false, false
	}
}

// gommoPollJob polls a status endpoint until the job produces a URL, fails, or
// the deadline passes.
//
// A ready URL is treated as success even when the status string is unfamiliar:
// the artifact is what the caller asked for, and status vocabularies drift.
// On deadline the job id is returned in the error so the operator can retrieve
// the result later rather than losing a paid render.
func gommoPollJob(parent *http.Request, account *config.Account, path string,
	params map[string]interface{}, extract func([]byte) gommoJob) (gommoJob, error) {

	deadline := time.Now().Add(gommoPollTimeout)
	var last gommoJob
	for attempt := 0; ; attempt++ {
		if parent != nil {
			select {
			case <-parent.Context().Done():
				return last, parent.Context().Err()
			default:
			}
		}

		raw, err := gommoPost(parent, account, path, params)
		if err != nil {
			return last, err
		}
		last = extract(raw)
		if strings.TrimSpace(last.URL) != "" {
			return last, nil
		}
		done, ok := gommoTerminalStatus(last.Status)
		if done && !ok {
			return last, &serviceHTTPError{
				Status: http.StatusBadGateway,
				Body:   fmt.Sprintf("gommo job %s ended with status %s", last.ID, last.Status),
			}
		}
		if done && ok {
			// Terminal-success with no URL is a provider inconsistency rather
			// than something to keep polling for.
			return last, &serviceHTTPError{
				Status: http.StatusBadGateway,
				Body:   fmt.Sprintf("gommo job %s reported %s but returned no media URL", last.ID, last.Status),
			}
		}
		if time.Now().After(deadline) {
			return last, &serviceHTTPError{
				Status: http.StatusGatewayTimeout,
				Body:   fmt.Sprintf("gommo job %s still %s after %s (retrieve it later with id_base=%s)", last.ID, firstNonEmpty(last.Status, "pending"), gommoPollTimeout, last.ID),
			}
		}
		if attempt == 0 {
			logger.Infof("[Gommo] polling job %s on %s", last.ID, path)
		}
		time.Sleep(gommoPollInterval)
	}
}

// ==================== Image generation ====================

// gommoProjectID resolves the project a generated artifact is filed under.
// "default" is the provider's own default project name.
func gommoProjectID(account *config.Account) string {
	if account != nil {
		if id := strings.TrimSpace(account.GommoProjectID); id != "" {
			return id
		}
	}
	return "default"
}

// gommoImageRatio maps an OpenAI `size` value onto Gommo's ratio vocabulary.
// Gommo accepts only a fixed set of ratios, not pixel dimensions, so an
// unrecognised size is dropped rather than forwarded and rejected.
func gommoImageRatio(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "", "auto":
		return ""
	case "9_16", "16_9", "1_1":
		return size
	case "9:16":
		return "9_16"
	case "16:9":
		return "16_9"
	case "1:1":
		return "1_1"
	}
	width, height, ok := parseImageSize(size)
	if !ok {
		return ""
	}
	switch {
	case width == height:
		return "1_1"
	case width > height:
		return "16_9"
	default:
		return "9_16"
	}
}

// parseImageSize reads a "1024x1024" style dimension pair.
func parseImageSize(size string) (int, int, bool) {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var width, height int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &width); err != nil || width <= 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &height); err != nil || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// gommoImageInfo reads the image job fields, accepting both the create response
// (nested under imageInfo) and the status response (top level).
func gommoImageInfo(raw []byte) gommoJob {
	var envelope struct {
		ImageInfo struct {
			IDBase string `json:"id_base"`
			Status string `json:"status"`
			URL    string `json:"url"`
			Prompt string `json:"prompt"`
		} `json:"imageInfo"`
		IDBase string `json:"id_base"`
		Status string `json:"status"`
		URL    string `json:"url"`
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return gommoJob{}
	}
	return gommoJob{
		ID:     firstNonEmpty(envelope.ImageInfo.IDBase, envelope.IDBase),
		Status: firstNonEmpty(envelope.ImageInfo.Status, envelope.Status),
		URL:    firstNonEmpty(envelope.ImageInfo.URL, envelope.URL),
		Prompt: firstNonEmpty(envelope.ImageInfo.Prompt, envelope.Prompt),
	}
}

// callGommoImage serves an OpenAI Images request from Gommo. The provider is
// asynchronous, so this creates the job and then polls until a URL exists.
//
// `n` is not forwarded: the API generates one image per call, so multiple
// images are separate create calls. They run sequentially because each one
// spends credit and a partial failure should stop rather than fan out.
func callGommoImage(parent *http.Request, account *config.Account, in imageGenerationRequest) (imageGenerationResponse, error) {
	if account == nil {
		return imageGenerationResponse{}, fmt.Errorf("gommo image: account is nil")
	}
	count := in.N
	if count <= 0 {
		count = 1
	}
	if count > 4 {
		// Each image is a separate paid job; cap the fan-out a single request
		// can trigger rather than spending an unbounded amount of credit.
		count = 4
	}

	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = strings.TrimSpace(account.ImageModel)
	}
	if model == "" {
		return imageGenerationResponse{}, fmt.Errorf("gommo image: no model configured for account %s", account.Email)
	}

	result := imageGenerationResponse{Created: time.Now().Unix()}
	for i := 0; i < count; i++ {
		params := map[string]interface{}{
			"action_type": "create",
			"model":       model,
			"prompt":      in.Prompt,
			"project_id":  gommoProjectID(account),
		}
		if ratio := gommoImageRatio(in.Size); ratio != "" {
			params["ratio"] = ratio
		}

		raw, err := gommoPost(parent, account, gommoPathCreateImage, params)
		if err != nil {
			if i > 0 && len(result.Data) > 0 {
				// Return what was already paid for instead of discarding it.
				logger.Warnf("[Gommo] image %d/%d failed for %s: %v", i+1, count, account.Email, err)
				break
			}
			return imageGenerationResponse{}, err
		}
		job := gommoImageInfo(raw)
		if strings.TrimSpace(job.URL) == "" {
			if strings.TrimSpace(job.ID) == "" {
				return imageGenerationResponse{}, &serviceHTTPError{
					Status: http.StatusBadGateway,
					Body:   "gommo image create returned neither a url nor an id_base",
				}
			}
			job, err = gommoPollJob(parent, account, gommoPathImage,
				map[string]interface{}{"id_base": job.ID}, gommoImageInfo)
			if err != nil {
				if i > 0 && len(result.Data) > 0 {
					logger.Warnf("[Gommo] image %d/%d poll failed for %s: %v", i+1, count, account.Email, err)
					break
				}
				return imageGenerationResponse{}, err
			}
		}
		result.Data = append(result.Data, imageGenerationData{URL: job.URL})
	}

	if len(result.Data) == 0 {
		return imageGenerationResponse{}, &serviceHTTPError{
			Status: http.StatusBadGateway,
			Body:   "gommo returned no image data",
		}
	}
	return result, nil
}

// ==================== Text to speech ====================

// gommoTTSRequest is the subset of the OpenAI /v1/audio/speech body that maps
// onto Gommo's createAudio call.
type gommoTTSRequest struct {
	Model          string  `json:"model,omitempty"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"`
}

// gommoTTSModel resolves the synthesis model. Gommo accepts only its own
// identifiers here, so an OpenAI model name (tts-1, gpt-4o-mini-tts) is replaced
// rather than forwarded: sending it produces an upstream validation error.
func gommoTTSModel(account *config.Account, requested string) string {
	requested = strings.TrimSpace(requested)
	if strings.HasPrefix(requested, "eleven_") {
		return requested
	}
	if account != nil {
		if mapped := strings.TrimSpace(account.ModelMappings[requested]); mapped != "" {
			return mapped
		}
		if configured := strings.TrimSpace(account.GommoTTSModel); configured != "" {
			return configured
		}
	}
	return "eleven_flash_v2_5"
}

// callGommoTTS synthesises speech and returns the raw audio bytes together with
// its content type, so the caller can answer /v1/audio/speech with a body of the
// same shape OpenAI returns.
//
// Gommo answers with a URL rather than inline audio, so the file is fetched on
// the caller's behalf. Handing the client a bare URL would leak the upstream
// endpoint and break every client that expects audio bytes.
func callGommoTTS(parent *http.Request, account *config.Account, in gommoTTSRequest) ([]byte, string, error) {
	text := strings.TrimSpace(in.Input)
	if text == "" {
		return nil, "", fmt.Errorf("input is required")
	}
	voice := strings.TrimSpace(in.Voice)
	if voice == "" {
		voice = strings.TrimSpace(account.GommoVoiceID)
	}
	if voice == "" {
		return nil, "", &unsupportedCapabilityError{
			Capability: "audio-tts",
			Account:    account.Email,
			Reason:     "gommo speech requires a voice id: pass \"voice\" or configure a default voice on the account",
		}
	}

	params := map[string]interface{}{
		"action_type": "createAudio",
		"text":        text,
		"voice_id":    voice,
		"model":       gommoTTSModel(account, in.Model),
		"project_id":  gommoProjectID(account),
	}
	if in.Speed > 0 {
		params["voice_settings[speed]"] = in.Speed
	}

	raw, err := gommoPost(parent, account, gommoPathAudio, params)
	if err != nil {
		return nil, "", err
	}

	var envelope struct {
		AudioInfo struct {
			IDBase  string  `json:"id_base"`
			Status  string  `json:"status"`
			FileURL string  `json:"file_url"`
			Price   float64 `json:"price"`
		} `json:"audioInfo"`
		BalancesInfo struct {
			CreditsAI float64 `json:"credits_ai"`
		} `json:"balancesInfo"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", fmt.Errorf("gommo audio parse: %w", err)
	}
	if envelope.BalancesInfo.CreditsAI > 0 {
		_ = config.UpdateAccountGommoCredits(account.ID, envelope.BalancesInfo.CreditsAI, time.Now().Unix())
	}
	fileURL := strings.TrimSpace(envelope.AudioInfo.FileURL)
	if fileURL == "" {
		return nil, "", &serviceHTTPError{
			Status: http.StatusBadGateway,
			Body:   fmt.Sprintf("gommo audio job %s returned no file_url (status %s)", envelope.AudioInfo.IDBase, envelope.AudioInfo.Status),
		}
	}
	return gommoFetchMedia(parent, account, fileURL)
}

// gommoFetchMedia downloads a generated artifact. The URL comes from the
// upstream's own response, and only https is followed: an http URL would move a
// credentialed workflow's output over plaintext.
func gommoFetchMedia(parent *http.Request, account *config.Account, mediaURL string) ([]byte, string, error) {
	parsed, err := url.Parse(mediaURL)
	if err != nil {
		return nil, "", fmt.Errorf("gommo media url: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, "", fmt.Errorf("gommo media url is not https: %s", parsed.Scheme)
	}

	var req *http.Request
	if parent != nil {
		req, err = http.NewRequestWithContext(parent.Context(), http.MethodGet, mediaURL, nil)
	} else {
		req, err = http.NewRequest(http.MethodGet, mediaURL, nil)
	}
	if err != nil {
		return nil, "", fmt.Errorf("gommo media request: %w", err)
	}
	resp, err := GetImageClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("gommo media fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &serviceHTTPError{Status: resp.StatusCode, Body: "gommo media fetch failed"}
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxGommoMediaBytes))
	if err != nil {
		return nil, "", fmt.Errorf("gommo media read: %w", err)
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	return payload, contentType, nil
}

// ==================== Video ====================

// gommoVideoRequest is the normalized video-generation input. It mirrors the
// OpenAI video vocabulary where one exists and falls back to Gommo's own field
// names for the controls that have no equivalent (mode, ratio).
type gommoVideoRequest struct {
	Prompt     string   `json:"prompt"`
	Model      string   `json:"model,omitempty"`
	Ratio      string   `json:"ratio,omitempty"`
	Resolution string   `json:"resolution,omitempty"`
	Duration   string   `json:"duration,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Privacy    string   `json:"privacy,omitempty"`
	Images     []string `json:"images,omitempty"`
	// TranslateToEn asks Gommo to translate the prompt before generation. The
	// Veo models do not accept Vietnamese, so this is on by default and can be
	// switched off explicitly.
	TranslateToEn *bool `json:"translate_to_en,omitempty"`
}

const gommoDefaultVideoModel = "veo_3_fast"

// endpointVideo is the usage/telemetry label for video generation.
const endpointVideo = "video"

// gommoVideoStatus reads one job's current state without waiting. It exists so a
// caller whose create call outlived the poll ceiling can retrieve the result
// later instead of paying for the render twice.
func gommoVideoStatus(parent *http.Request, account *config.Account, jobID string) (gommoJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return gommoJob{}, fmt.Errorf("gommo video status: job id is required")
	}
	raw, err := gommoPost(parent, account, gommoPathVideo, map[string]interface{}{"videoId": jobID})
	if err != nil {
		return gommoJob{}, err
	}
	var status struct {
		IDBase       string `json:"id_base"`
		Status       string `json:"status"`
		DownloadURL  string `json:"download_url"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return gommoJob{}, fmt.Errorf("gommo video status parse: %w", err)
	}
	return gommoJob{
		ID:     firstNonEmpty(strings.TrimSpace(status.IDBase), jobID),
		Status: status.Status,
		URL:    strings.TrimSpace(status.DownloadURL),
	}, nil
}

// callGommoVideo creates a video job and waits for its download URL.
func callGommoVideo(parent *http.Request, account *config.Account, in gommoVideoRequest) (gommoJob, error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return gommoJob{}, fmt.Errorf("gommo video: prompt is required")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = gommoDefaultVideoModel
	}
	privacy := strings.ToUpper(strings.TrimSpace(in.Privacy))
	if privacy != "PUBLIC" {
		// Default to PRIVATE: generated media should not become publicly
		// listable unless the caller asked for it.
		privacy = "PRIVATE"
	}
	translate := true
	if in.TranslateToEn != nil {
		translate = *in.TranslateToEn
	}

	params := map[string]interface{}{
		"model":           model,
		"privacy":         privacy,
		"prompt":          prompt,
		"translate_to_en": translate,
		"project_id":      gommoProjectID(account),
		"ratio":           in.Ratio,
		"resolution":      in.Resolution,
		"duration":        in.Duration,
		"mode":            in.Mode,
	}
	if len(in.Images) > 0 {
		params["images"] = in.Images
	}

	raw, err := gommoPost(parent, account, gommoPathCreateVideo, params)
	if err != nil {
		return gommoJob{}, err
	}
	var created struct {
		VideoInfo struct {
			IDBase    string  `json:"id_base"`
			TaskID    string  `json:"task_id"`
			Status    string  `json:"status"`
			CreditFee float64 `json:"credit_fee"`
		} `json:"videoInfo"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return gommoJob{}, fmt.Errorf("gommo video create parse: %w", err)
	}
	jobID := strings.TrimSpace(created.VideoInfo.IDBase)
	if jobID == "" {
		return gommoJob{}, &serviceHTTPError{Status: http.StatusBadGateway, Body: "gommo video create returned no id_base"}
	}

	return gommoPollJob(parent, account, gommoPathVideo,
		map[string]interface{}{"videoId": jobID},
		func(body []byte) gommoJob {
			var status struct {
				IDBase      string `json:"id_base"`
				Status      string `json:"status"`
				DownloadURL string `json:"download_url"`
			}
			if json.Unmarshal(body, &status) != nil {
				return gommoJob{ID: jobID}
			}
			return gommoJob{
				ID:     firstNonEmpty(strings.TrimSpace(status.IDBase), jobID),
				Status: status.Status,
				URL:    strings.TrimSpace(status.DownloadURL),
			}
		})
}

// ==================== Model catalog ====================

// gommoModelTypes are the catalog partitions the provider exposes. Each is a
// separate call because /ai/models takes exactly one type per request.
var gommoModelTypes = []struct {
	apiType    string
	capability string
}{
	{"image", capabilityImage},
	{"video", capabilityVideo},
	{"tts", capabilityAudioTTS},
}

// fetchGommoModels reads the provider's own catalog for every media type. A
// failing partition is skipped rather than failing the whole refresh: the
// account still serves the types that answered.
func fetchGommoModels(account *config.Account) ([]ModelInfo, error) {
	var out []ModelInfo
	var lastErr error
	for _, partition := range gommoModelTypes {
		raw, err := gommoPost(nil, account, gommoPathModels, map[string]interface{}{"type": partition.apiType})
		if err != nil {
			lastErr = err
			logger.Warnf("[Gommo] model catalog %s failed for %s: %v", partition.apiType, account.Email, err)
			continue
		}
		out = append(out, parseGommoModels(raw, partition.apiType)...)
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// parseGommoModels normalizes one catalog partition. The entries are free-form
// objects whose id field is spelled differently per type, so several keys are
// accepted before an entry is discarded.
func parseGommoModels(raw []byte, mediaType string) []ModelInfo {
	var envelope struct {
		Data []map[string]interface{} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	outputType := mediaType
	if mediaType == "tts" {
		outputType = "audio"
	}
	out := make([]ModelInfo, 0, len(envelope.Data))
	seen := make(map[string]bool)
	for _, entry := range envelope.Data {
		id := ""
		for _, key := range []string{"model", "id", "model_id", "name", "value"} {
			if text, ok := entry[key].(string); ok && strings.TrimSpace(text) != "" {
				id = strings.TrimSpace(text)
				break
			}
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := id
		if label, ok := entry["title"].(string); ok && strings.TrimSpace(label) != "" {
			name = strings.TrimSpace(label)
		} else if label, ok := entry["name"].(string); ok && strings.TrimSpace(label) != "" {
			name = strings.TrimSpace(label)
		}
		out = append(out, ModelInfo{
			ModelId:        id,
			ModelName:      name,
			Description:    fmt.Sprintf("Gommo %s model", mediaType),
			Provider:       gommoProviderLabel,
			OutputTypes:    []string{outputType},
			Modalities:     []string{outputType},
			RateMultiplier: 1.0,
		})
	}
	return out
}

// ==================== Account info ====================

// refreshGommoAccount reads the account endpoint and persists the balances the
// admin UI shows. credits_ai is the balance the generative endpoints draw down.
func refreshGommoAccount(account *config.Account) error {
	if !isGommoAccount(account) {
		return fmt.Errorf("account %s is not a Gommo account", account.ID)
	}
	raw, err := gommoPost(nil, account, gommoPathMe, nil)
	if err != nil {
		return err
	}
	var envelope struct {
		UserInfo struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"userInfo"`
		BalancesInfo struct {
			Balance   float64 `json:"balance"`
			CreditsAI float64 `json:"credits_ai"`
			Currency  string  `json:"currency"`
		} `json:"balancesInfo"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("gommo account parse: %w", err)
	}

	account.GommoCreditsAI = envelope.BalancesInfo.CreditsAI
	account.GommoBalance = envelope.BalancesInfo.Balance
	if envelope.BalancesInfo.Currency != "" {
		account.GommoCurrency = envelope.BalancesInfo.Currency
	}
	now := time.Now().Unix()
	account.GommoCreditsCheckedAt = now
	if account.Nickname == "" {
		account.Nickname = firstNonEmpty(envelope.UserInfo.Name, envelope.UserInfo.Username)
	}
	return config.UpdateAccountGommoBalances(account.ID,
		envelope.BalancesInfo.CreditsAI, envelope.BalancesInfo.Balance,
		envelope.BalancesInfo.Currency, now)
}
